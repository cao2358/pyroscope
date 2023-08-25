package phlaredb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/status"
	"github.com/google/uuid"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/samber/lo"
	"go.uber.org/atomic"
	"google.golang.org/grpc/codes"

	profilev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
	ingestv1 "github.com/grafana/pyroscope/api/gen/proto/go/ingester/v1"
	typesv1 "github.com/grafana/pyroscope/api/gen/proto/go/types/v1"
	"github.com/grafana/pyroscope/pkg/iter"
	phlaremodel "github.com/grafana/pyroscope/pkg/model"
	phlarecontext "github.com/grafana/pyroscope/pkg/phlare/context"
	"github.com/grafana/pyroscope/pkg/phlaredb/block"
	schemav1 "github.com/grafana/pyroscope/pkg/phlaredb/schemas/v1"
	"github.com/grafana/pyroscope/pkg/phlaredb/symdb"
)

var defaultParquetConfig = &ParquetConfig{
	MaxBufferRowCount: 100_000,
	MaxRowGroupBytes:  10 * 128 * 1024 * 1024,
	MaxBlockBytes:     10 * 10 * 128 * 1024 * 1024,
}

type Table interface {
	Name() string
	Size() uint64       // Size estimates the uncompressed byte size of the table in memory and on disk.
	MemorySize() uint64 // MemorySize estimates the uncompressed byte size of the table in memory.
	Init(path string, cfg *ParquetConfig, metrics *headMetrics) error
	Flush(context.Context) (numRows uint64, numRowGroups uint64, err error)
	Close() error
}

type Head struct {
	logger  log.Logger
	metrics *headMetrics
	stopCh  chan struct{}
	wg      sync.WaitGroup

	headPath  string // path while block is actively appended to
	localPath string // path once block has been cut

	inFlightProfiles sync.WaitGroup // ongoing ingestion requests.
	flushCh          chan struct{}  // this channel is closed once the Head should be flushed, should be used externally
	flushForcedTimer *time.Timer    // this timer will phlare after the maximum

	metaLock sync.RWMutex
	meta     *block.Meta

	parquetConfig *ParquetConfig
	symdb         *symdb.SymDB
	profiles      *profileStore
	totalSamples  *atomic.Uint64
	tables        []Table
	delta         *deltaProfiles

	limiter TenantLimiter
}

const (
	pathHead          = "head"
	pathLocal         = "local"
	defaultFolderMode = 0o755
)

func NewHead(phlarectx context.Context, cfg Config, limiter TenantLimiter) (*Head, error) {
	// todo if tenantLimiter is nil ....
	parquetConfig := *defaultParquetConfig
	h := &Head{
		logger:  phlarecontext.Logger(phlarectx),
		metrics: contextHeadMetrics(phlarectx),

		stopCh: make(chan struct{}),

		meta:         block.NewMeta(),
		totalSamples: atomic.NewUint64(0),

		flushCh:          make(chan struct{}),
		flushForcedTimer: time.NewTimer(cfg.MaxBlockDuration),

		parquetConfig: &parquetConfig,
		limiter:       limiter,
	}
	h.headPath = filepath.Join(cfg.DataPath, pathHead, h.meta.ULID.String())
	h.localPath = filepath.Join(cfg.DataPath, pathLocal, h.meta.ULID.String())

	if cfg.Parquet != nil {
		h.parquetConfig = cfg.Parquet
	}

	h.parquetConfig.MaxRowGroupBytes = cfg.RowGroupTargetSize

	// ensure folder is writable
	err := os.MkdirAll(h.headPath, defaultFolderMode)
	if err != nil {
		return nil, err
	}

	// create profile store
	h.profiles = newProfileStore(phlarectx)
	h.delta = newDeltaProfiles()
	h.tables = []Table{
		h.profiles,
	}
	for _, t := range h.tables {
		if err := t.Init(h.headPath, h.parquetConfig, h.metrics); err != nil {
			return nil, err
		}
	}

	h.symdb = symdb.NewSymDB(symdb.DefaultConfig().
		WithDirectory(filepath.Join(h.headPath, symdb.DefaultDirName)).
		WithParquetConfig(symdb.ParquetConfig{
			MaxBufferRowCount: h.parquetConfig.MaxBufferRowCount,
		}))

	h.wg.Add(1)
	go h.loop()

	return h, nil
}

func (h *Head) MemorySize() uint64 {
	// TODO: TSDB index
	return h.profiles.MemorySize() + h.symdb.MemorySize()
}

func (h *Head) Size() uint64 {
	// TODO: TSDB and SymDB.
	return h.profiles.Size()
}

func (h *Head) loop() {
	tick := time.NewTicker(5 * time.Second)
	symdbMetricsUpdateTicker := time.NewTicker(5 * time.Second)
	var memStats symdb.MemoryStats
	defer func() {
		tick.Stop()
		symdbMetricsUpdateTicker.Stop()
		h.flushForcedTimer.Stop()
		h.wg.Done()
	}()

	for {
		select {
		case <-h.flushForcedTimer.C:
			h.metrics.flushedBlocksReasons.WithLabelValues("max-duration").Inc()
			level.Debug(h.logger).Log("msg", "max block duration reached, flush to disk")
			close(h.flushCh)
			return

		case <-tick.C:
			if currentSize := h.Size(); currentSize > h.parquetConfig.MaxBlockBytes {
				h.metrics.flushedBlocksReasons.WithLabelValues("max-block-bytes").Inc()
				level.Debug(h.logger).Log(
					"msg", "max block bytes reached, flush to disk",
					"max_size", humanize.Bytes(h.parquetConfig.MaxBlockBytes),
					"current_head_size", humanize.Bytes(currentSize),
				)
				close(h.flushCh)
				return
			}

		case <-symdbMetricsUpdateTicker.C:
			h.updateSymbolsMemUsage(&memStats)

		case <-h.stopCh:
			return
		}
	}
}

func (h *Head) Ingest(ctx context.Context, p *profilev1.Profile, id uuid.UUID, externalLabels ...*typesv1.LabelPair) error {
	labels, seriesFingerprints := labelsForProfile(p, externalLabels...)

	for i, fp := range seriesFingerprints {
		if err := h.limiter.AllowProfile(fp, labels[i], p.TimeNanos); err != nil {
			return err
		}
	}

	// determine the stacktraces partition ID
	partition := phlaremodel.StacktracePartitionFromProfile(labels, p)

	metricName := phlaremodel.Labels(externalLabels).Get(model.MetricNameLabel)

	var profileIngested bool
	for idxType, profile := range h.symdb.WriteProfileSymbols(partition, p) {
		profile.ID = id
		profile.SeriesFingerprint = seriesFingerprints[idxType]
		profile.Samples = h.delta.computeDelta(profile, labels[idxType])
		profile.TotalValue = profile.Samples.Sum()

		if profile.Samples.Len() == 0 {
			level.Debug(h.logger).Log("msg", "profile is empty after delta computation", "metricName", metricName)
			continue
		}

		if err := h.profiles.ingest(ctx, []schemav1.InMemoryProfile{profile}, labels[idxType], metricName); err != nil {
			return err
		}

		profileIngested = true
		h.totalSamples.Add(uint64(profile.Samples.Len()))
		h.metrics.sampleValuesIngested.WithLabelValues(metricName).Add(float64(profile.Samples.Len()))
		h.metrics.sampleValuesReceived.WithLabelValues(metricName).Add(float64(len(p.Sample)))
	}

	if !profileIngested {
		return nil
	}

	h.metaLock.Lock()
	v := model.TimeFromUnixNano(p.TimeNanos)
	if v < h.meta.MinTime {
		h.meta.MinTime = v
	}
	if v > h.meta.MaxTime {
		h.meta.MaxTime = v
	}
	h.metaLock.Unlock()

	return nil
}

// LabelValues returns the possible label values for a given label name.
func (h *Head) LabelValues(ctx context.Context, req *connect.Request[typesv1.LabelValuesRequest]) (*connect.Response[typesv1.LabelValuesResponse], error) {
	selectors, err := parseSelectors(req.Msg.Matchers)
	if err != nil {
		return nil, err
	}

	// shortcut to index when matcher match all
	if selectors.matchesAll() {
		values, err := h.profiles.index.ix.LabelValues(req.Msg.Name, nil)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&typesv1.LabelValuesResponse{
			Names: values,
		}), nil
	}

	// aggregate all label values from series matching, when matchers are given.

	values := make(map[string]struct{})
	if err := h.forMatchingSelectors(selectors, func(lbs phlaremodel.Labels, fp model.Fingerprint) error {
		if v := lbs.Get(req.Msg.Name); v != "" {
			values[v] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return connect.NewResponse(&typesv1.LabelValuesResponse{
		Names: lo.Keys(values),
	}), nil
}

// LabelNames returns the possible label values for a given label name.
func (h *Head) LabelNames(ctx context.Context, req *connect.Request[typesv1.LabelNamesRequest]) (*connect.Response[typesv1.LabelNamesResponse], error) {
	selectors, err := parseSelectors(req.Msg.Matchers)
	if err != nil {
		return nil, err
	}

	// shortcut to index when matcher match all
	if selectors.matchesAll() {
		values, err := h.profiles.index.ix.LabelNames(nil)
		if err != nil {
			return nil, err
		}
		sort.Strings(values)
		return connect.NewResponse(&typesv1.LabelNamesResponse{
			Names: values,
		}), nil
	}

	// aggregate all label values from series matching, when matchers are given.
	values := make(map[string]struct{})
	if err := h.forMatchingSelectors(selectors, func(lbs phlaremodel.Labels, fp model.Fingerprint) error {
		for _, lbl := range lbs {
			values[lbl.Name] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return connect.NewResponse(&typesv1.LabelNamesResponse{
		Names: lo.Keys(values),
	}), nil
}

// ProfileTypes returns the possible profile types.
func (h *Head) ProfileTypes(ctx context.Context, req *connect.Request[ingestv1.ProfileTypesRequest]) (*connect.Response[ingestv1.ProfileTypesResponse], error) {
	values, err := h.profiles.index.ix.LabelValues(phlaremodel.LabelNameProfileType, nil)
	if err != nil {
		return nil, err
	}
	sort.Strings(values)

	profileTypes := make([]*typesv1.ProfileType, len(values))
	for i, v := range values {
		tp, err := phlaremodel.ParseProfileTypeSelector(v)
		if err != nil {
			return nil, err
		}
		profileTypes[i] = tp
	}

	return connect.NewResponse(&ingestv1.ProfileTypesResponse{
		ProfileTypes: profileTypes,
	}), nil
}

func (h *Head) Bounds() (mint, maxt model.Time) {
	h.metaLock.RLock()
	defer h.metaLock.RUnlock()
	return h.meta.MinTime, h.meta.MaxTime
}

// Returns underlying queries, the queriers should be roughly ordered in TS increasing order
func (h *Head) Queriers() Queriers {
	h.profiles.rowsLock.RLock()
	defer h.profiles.rowsLock.RUnlock()

	queriers := make([]Querier, 0, len(h.profiles.rowGroups)+1)
	for idx := range h.profiles.rowGroups {
		queriers = append(queriers, &headOnDiskQuerier{
			head:        h,
			rowGroupIdx: idx,
		})
	}
	queriers = append(queriers, &headInMemoryQuerier{h})
	return queriers
}

func (h *Head) Sort(in []Profile) []Profile {
	return in
}

type ProfileSelectorIterator struct {
	batch   chan []Profile
	current iter.Iterator[Profile]
	once    sync.Once
}

func NewProfileSelectorIterator() *ProfileSelectorIterator {
	return &ProfileSelectorIterator{
		batch: make(chan []Profile, 1),
	}
}

func (it *ProfileSelectorIterator) Push(batch []Profile) {
	if len(batch) == 0 {
		return
	}
	it.batch <- batch
}

func (it *ProfileSelectorIterator) Next() bool {
	if it.current == nil {
		batch, ok := <-it.batch
		if !ok {
			return false
		}
		it.current = iter.NewSliceIterator(batch)
	}
	if !it.current.Next() {
		it.current = nil
		return it.Next()
	}
	return true
}

func (it *ProfileSelectorIterator) At() Profile {
	if it.current == nil {
		return ProfileWithLabels{}
	}
	return it.current.At()
}

func (it *ProfileSelectorIterator) Close() error {
	it.once.Do(func() {
		close(it.batch)
	})
	return nil
}

func (it *ProfileSelectorIterator) Err() error {
	return nil
}

// selectors are composed of any amount of selectors which are ORed
type selectors [][]*labels.Matcher

func parseSelectors(selectorStrings []string) (selectors, error) {
	sels := make([][]*labels.Matcher, 0, len(selectorStrings))
	for _, m := range selectorStrings {
		s, err := parser.ParseMetricSelector(m)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("failed to parse label selector: %v", err))
		}
		sels = append(sels, s)
	}

	return sels, nil
}

func (sels selectors) matchesAll() bool {
	if len(sels) == 0 {
		return true
	}

	for _, sel := range sels {
		if len(sel) == 0 {
			return true
		}
	}

	return false
}

func (h *Head) forMatchingSelectors(sels selectors, fn func(lbs phlaremodel.Labels, fp model.Fingerprint) error) error {
	if sels.matchesAll() {
		return h.profiles.index.forMatchingLabels(nil, fn)
	}

	for _, sel := range sels {
		if err := h.profiles.index.forMatchingLabels(sel, fn); err != nil {
			return err
		}
	}

	return nil
}

func (h *Head) Series(ctx context.Context, req *connect.Request[ingestv1.SeriesRequest]) (*connect.Response[ingestv1.SeriesResponse], error) {
	selectors, err := parseSelectors(req.Msg.Matchers)
	if err != nil {
		return nil, err
	}

	// build up map of label names
	labelNameMap := make(map[string]struct{}, len(req.Msg.LabelNames))
	for _, labelName := range req.Msg.LabelNames {
		labelNameMap[labelName] = struct{}{}
	}

	response := &ingestv1.SeriesResponse{}
	uniqu := map[model.Fingerprint]struct{}{}
	if err := h.forMatchingSelectors(selectors, func(lbs phlaremodel.Labels, fp model.Fingerprint) error {
		if len(req.Msg.LabelNames) > 0 {
			lbs = lbs.WithLabels(req.Msg.LabelNames...)
			fp = model.Fingerprint(lbs.Hash())

		}

		if _, ok := uniqu[fp]; ok {
			return nil
		}
		uniqu[fp] = struct{}{}
		response.LabelsSet = append(response.LabelsSet, &typesv1.Labels{Labels: lbs})
		return nil
	}); err != nil {
		return nil, err
	}

	sort.Slice(response.LabelsSet, func(i, j int) bool {
		return phlaremodel.CompareLabelPairs(response.LabelsSet[i].Labels, response.LabelsSet[j].Labels) < 0
	})
	return connect.NewResponse(response), nil
}

// Flush closes the head and writes data to disk. No ingestion requests should
// be made concurrently with the call, or after it returns.
// The call is thread-safe for reads.
func (h *Head) Flush(ctx context.Context) error {
	close(h.stopCh)
	h.wg.Wait()
	start := time.Now()
	defer func() {
		h.metrics.flushedBlockDurationSeconds.Observe(time.Since(start).Seconds())
	}()
	if err := h.flush(ctx); err != nil {
		h.metrics.flushedBlocks.WithLabelValues("failed").Inc()
		return err
	}
	h.metrics.flushedBlocks.WithLabelValues("success").Inc()
	return nil
}

func (h *Head) flush(ctx context.Context) error {
	// Ensure all the in-flight ingestion requests have finished.
	// It must be guaranteed that no new inserts will happen
	// after the call start.
	h.inFlightProfiles.Wait()
	if len(h.profiles.slice) == 0 {
		level.Info(h.logger).Log("msg", "head empty - no block written")
		return os.RemoveAll(h.headPath)
	}

	var blockSize uint64
	files := make([]block.File, 0, 8)

	// profiles
	for _, t := range h.tables {
		numRows, numRowGroups, err := t.Flush(ctx)
		if err != nil {
			return errors.Wrapf(err, "flushing table %s", t.Name())
		}
		h.metrics.rowsWritten.WithLabelValues(t.Name()).Add(float64(numRows))
		f := block.File{
			RelPath: t.Name() + block.ParquetSuffix,
			Parquet: &block.ParquetFile{
				NumRowGroups: numRowGroups,
				NumRows:      numRows,
			},
		}
		if err = t.Close(); err != nil {
			return errors.Wrapf(err, "closing table %s", t.Name())
		}
		if stat, err := os.Stat(filepath.Join(h.headPath, f.RelPath)); err == nil {
			f.SizeBytes = uint64(stat.Size())
			blockSize += f.SizeBytes
			h.metrics.flushedFileSizeBytes.WithLabelValues(t.Name()).Observe(float64(f.SizeBytes))
		}
		files = append(files, f)
	}

	// symdb
	if err := h.symdb.Flush(); err != nil {
		return errors.Wrap(err, "flushing symdb")
	}
	for _, file := range h.symdb.Files() {
		// Files' path is relative to the symdb dir.
		file.RelPath = filepath.Join(symdb.DefaultDirName, file.RelPath)
		files = append(files, file)
		blockSize += file.SizeBytes
		h.metrics.flushedFileSizeBytes.WithLabelValues(file.RelPath).Observe(float64(file.SizeBytes))
	}

	// tsdb
	h.meta.Stats.NumSeries = uint64(h.profiles.index.totalSeries.Load())
	f := block.File{
		RelPath: block.IndexFilename,
		TSDB: &block.TSDBFile{
			NumSeries: h.meta.Stats.NumSeries,
		},
	}
	h.metrics.flushedBlockSeries.Observe(float64(h.meta.Stats.NumSeries))
	if stat, err := os.Stat(filepath.Join(h.headPath, block.IndexFilename)); err == nil {
		f.SizeBytes = uint64(stat.Size())
		blockSize += f.SizeBytes
		h.metrics.flushedFileSizeBytes.WithLabelValues("tsdb").Observe(float64(f.SizeBytes))
	}
	files = append(files, f)

	h.metrics.flushedBlockSizeBytes.Observe(float64(blockSize))
	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})
	h.meta.Files = files
	h.meta.Stats.NumProfiles = uint64(h.profiles.index.totalProfiles.Load())
	h.meta.Stats.NumSamples = h.totalSamples.Load()
	h.meta.Compaction.Sources = []ulid.ULID{h.meta.ULID}
	h.meta.Compaction.Level = 1
	h.metrics.flushedBlockSamples.Observe(float64(h.meta.Stats.NumSamples))
	h.metrics.flusehdBlockProfiles.Observe(float64(h.meta.Stats.NumProfiles))

	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	if _, err := h.meta.WriteToFile(h.logger, h.headPath); err != nil {
		return err
	}
	h.metrics.blockDurationSeconds.Observe(h.meta.MaxTime.Sub(h.meta.MinTime).Seconds())
	return nil
}

// Move moves the head directory to local blocks. The call is not thread-safe:
// no concurrent reads and writes are allowed.
//
// After the call, head in-memory representation is not valid and should not
// be accessed for querying.
func (h *Head) Move() error {
	// Remove intermediate row groups before the move as they are still
	// referencing files on the disk.
	if err := h.profiles.DeleteRowGroups(); err != nil {
		return err
	}

	// move block to the local directory
	if err := os.MkdirAll(filepath.Dir(h.localPath), defaultFolderMode); err != nil {
		return err
	}
	if err := fileutil.Rename(h.headPath, h.localPath); err != nil {
		return err
	}

	level.Info(h.logger).Log("msg", "head successfully written to block", "block_path", h.localPath)
	return nil
}

func (h *Head) updateSymbolsMemUsage(memStats *symdb.MemoryStats) {
	h.symdb.WriteMemoryStats(memStats)
	m := h.metrics.sizeBytes
	m.WithLabelValues("stacktraces").Set(float64(memStats.StacktracesSize))
	m.WithLabelValues("locations").Set(float64(memStats.LocationsSize))
	m.WithLabelValues("functions").Set(float64(memStats.FunctionsSize))
	m.WithLabelValues("mappings").Set(float64(memStats.MappingsSize))
	m.WithLabelValues("strings").Set(float64(memStats.StringsSize))
}
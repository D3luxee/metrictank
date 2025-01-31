package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/grafana/metrictank/api/models"
	"github.com/grafana/metrictank/consolidation"
	"github.com/grafana/metrictank/mdata"
	"github.com/grafana/metrictank/mdata/cache"
	"github.com/grafana/metrictank/mdata/chunk/tsz"
	"github.com/grafana/metrictank/schema"
	"github.com/grafana/metrictank/tracing"
	"github.com/grafana/metrictank/util"
	opentracing "github.com/opentracing/opentracing-go"
	tags "github.com/opentracing/opentracing-go/ext"
	traceLog "github.com/opentracing/opentracing-go/log"
	log "github.com/sirupsen/logrus"
)

// doRecover is the handler that turns panics into returns from the top level of getTarget.
func doRecover(errp *error) {
	e := recover()
	if e != nil {
		if _, ok := e.(runtime.Error); ok {
			panic(e)
		}
		if err, ok := e.(error); ok {
			*errp = err
		} else if errStr, ok := e.(string); ok {
			*errp = errors.New(errStr)
		} else {
			*errp = fmt.Errorf("%v", e)
		}
	}
	return
}

type getTargetsResp struct {
	series []models.Series
	err    error
}

// alignForward aligns ts to the next timestamp that divides by the interval, except if it is already aligned
func alignForward(ts, interval uint32) uint32 {
	remain := ts % interval
	if remain == 0 {
		return ts
	}
	return ts + interval - remain
}

// alignBackward aligns the ts to the previous ts that divides by the interval, even if it is already aligned
func alignBackward(ts uint32, interval uint32) uint32 {
	return ts - ((ts-1)%interval + 1)
}

// Fix assures a series is in quantized form:
// all points are nicely aligned (quantized) and padded with nulls in case there's gaps in data
// graphite does this quantization before storing, we may want to do that as well at some point
// note: values are quantized to the right because we can't lie about the future:
// e.g. if interval is 10 and we have a point at 8 or at 2, it will be quantized to 10, we should never move
// values to earlier in time.
func Fix(in []schema.Point, from, to, interval uint32) []schema.Point {

	first := alignForward(from, interval)
	last := alignBackward(to, interval)

	if last < first {
		// the requested range is too narrow for the requested interval
		return []schema.Point{}
	}
	// 3 attempts to get a sufficiently sized slice from the pool. if it fails, allocate a new one.
	var out []schema.Point
	neededCap := int((last-first)/interval + 1)
	for attempt := 1; attempt < 4; attempt++ {
		candidate := pointSlicePool.Get().([]schema.Point)
		if cap(candidate) >= neededCap {
			out = candidate[:neededCap]
			break
		}
		pointSlicePool.Put(candidate)
	}
	if out == nil {
		out = make([]schema.Point, neededCap)
	}

	// i iterates in. o iterates out. t is the ts we're looking to fill.
	for t, i, o := first, 0, -1; t <= last; t += interval {
		o += 1

		// input is out of values. add a null
		if i >= len(in) {
			out[o] = schema.Point{Val: math.NaN(), Ts: t}
			continue
		}

		p := in[i]
		if p.Ts == t {
			// point has perfect ts, use it and move on to next point
			out[o] = p
			i++
		} else if p.Ts > t {
			// point is too recent, append a null and reconsider same point for next slot
			out[o] = schema.Point{Val: math.NaN(), Ts: t}
		} else if p.Ts > t-interval {
			// point is older but not by more than 1 interval, so it's good enough, just quantize the ts, and move on to next point for next round
			out[o] = schema.Point{Val: p.Val, Ts: t}
			i++
		} else {
			// point is too old (older by 1 interval or more).
			// advance until we find a point that is recent enough, and then go through the considerations again,
			// if those considerations are any of the above ones.
			// if the last point would end up in this branch again, discard it as well.
			for p.Ts <= t-interval && i < len(in)-1 {
				i++
				p = in[i]
			}
			if p.Ts <= t-interval {
				i++
			}
			t -= interval
			o -= 1
		}
	}
	pointSlicePool.Put(in[:0])

	return out
}

// divideContext wraps a Consolidate() call with a context.Context condition
// important: pointsB will be released to the pool. do not keep a reference to it
func divideContext(ctx context.Context, pointsA, pointsB []schema.Point) []schema.Point {
	select {
	case <-ctx.Done():
		//request canceled
		return nil
	default:
	}
	return divide(pointsA, pointsB)
}

// divide divides pointsA by pointsB - pointwise. pointsA will be reused for the output.
// important: pointsB will be released to the pool. do not keep a reference to it
func divide(pointsA, pointsB []schema.Point) []schema.Point {
	if len(pointsA) != len(pointsB) {
		panic(fmt.Errorf("divide of a series with len %d by a series with len %d", len(pointsA), len(pointsB)))
	}
	for i := range pointsA {
		pointsA[i].Val /= pointsB[i].Val
	}
	pointSlicePool.Put(pointsB[:0])
	return pointsA
}

func (s *Server) getTargets(ctx context.Context, ss *models.StorageStats, reqs []models.Req) ([]models.Series, error) {
	// split reqs into local and remote.
	localReqs := make([]models.Req, 0)
	remoteReqs := make(map[string][]models.Req)
	for _, req := range reqs {
		if req.Node.IsLocal() {
			localReqs = append(localReqs, req)
		} else {
			remoteReqs[req.Node.GetName()] = append(remoteReqs[req.Node.GetName()], req)
		}
	}

	var wg sync.WaitGroup

	responses := make(chan getTargetsResp, 1)
	getCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if len(localReqs) > 0 {
		wg.Add(1)
		go func() {
			// the only errors returned are from us catching panics, so we should treat them
			// all as internalServerErrors
			series, err := s.getTargetsLocal(getCtx, ss, localReqs)
			if err != nil {
				cancel()
			}
			responses <- getTargetsResp{series, err}
			wg.Done()
		}()
	}
	if len(remoteReqs) > 0 {
		wg.Add(1)
		go func() {
			// all errors returned are *response.Error.
			series, err := s.getTargetsRemote(getCtx, ss, remoteReqs)
			if err != nil {
				cancel()
			}
			responses <- getTargetsResp{series, err}
			wg.Done()
		}()
	}

	// wait for all getTargets goroutines to end, then close our responses channel
	go func() {
		wg.Wait()
		close(responses)
	}()

	out := make([]models.Series, 0)
	for resp := range responses {
		if resp.err != nil {
			return nil, resp.err
		}
		out = append(out, resp.series...)
	}
	log.Debugf("DP getTargets: %d series found on cluster", len(out))
	return out, nil
}

// getTargetsRemote issues the requests on other nodes
// it's nothing more than a thin network wrapper around getTargetsLocal of a peer.
func (s *Server) getTargetsRemote(ctx context.Context, ss *models.StorageStats, remoteReqs map[string][]models.Req) ([]models.Series, error) {
	responses := make(chan getTargetsResp, len(remoteReqs))
	rCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg := sync.WaitGroup{}
	wg.Add(len(remoteReqs))
	for _, nodeReqs := range remoteReqs {
		log.Debugf("DP getTargetsRemote: handling %d reqs from %s", len(nodeReqs), nodeReqs[0].Node.GetName())
		go func(reqs []models.Req) {
			defer wg.Done()
			node := reqs[0].Node
			buf, err := node.Post(rCtx, "getTargetsRemote", "/getdata", models.GetData{Requests: reqs})
			if err != nil {
				cancel()
				responses <- getTargetsResp{nil, err}
				return
			}
			var resp models.GetDataRespV1
			_, err = resp.UnmarshalMsg(buf)
			if err != nil {
				cancel()
				log.Errorf("DP getTargetsRemote: error unmarshaling body from %s/getdata: %q", node.GetName(), err.Error())
				responses <- getTargetsResp{nil, err}
				return
			}
			log.Debugf("DP getTargetsRemote: %s returned %d series", node.GetName(), len(resp.Series))
			ss.Add(&resp.Stats)
			responses <- getTargetsResp{resp.Series, nil}
		}(nodeReqs)
	}

	// wait for all getTargetsRemote goroutines to end, then close our responses channel
	go func() {
		wg.Wait()
		close(responses)
	}()

	out := make([]models.Series, 0)
	for resp := range responses {
		if resp.err != nil {
			return nil, resp.err
		}
		out = append(out, resp.series...)
	}

	log.Debugf("DP getTargetsRemote: total of %d series found on peers", len(out))
	return out, nil
}

// error is the error of the first failing target request
func (s *Server) getTargetsLocal(ctx context.Context, ss *models.StorageStats, reqs []models.Req) ([]models.Series, error) {
	log.Debugf("DP getTargetsLocal: handling %d reqs locally", len(reqs))
	rCtx, span := tracing.NewSpan(ctx, s.Tracer, "getTargetsLocal")
	defer span.Finish()
	span.LogFields(traceLog.Int("num_reqs", len(reqs)))
	responses := make(chan getTargetsResp, len(reqs))

	var wg sync.WaitGroup
	reqLimiter := util.NewLimiter(getTargetsConcurrency)

	rCtx, cancel := context.WithCancel(rCtx)
	defer cancel()
LOOP:
	for _, req := range reqs {
		// if there are already getDataConcurrency goroutines running, then block
		// until a slot becomes free or our context is canceled.
		if !reqLimiter.Acquire(rCtx) {
			//request canceled
			break LOOP
		}
		wg.Add(1)
		go func(req models.Req) {
			pre := time.Now()
			points, interval, err := s.getTarget(rCtx, ss, req)
			if err != nil {
				cancel() // cancel all other requests.
				responses <- getTargetsResp{nil, err}
			} else {
				getTargetDuration.Value(time.Now().Sub(pre))
				responses <- getTargetsResp{[]models.Series{{
					Target:       req.Target, // always simply the metric name from index
					Datapoints:   points,
					Interval:     interval,
					QueryPatt:    req.Pattern, // foo.* or foo.bar whatever the etName arg was
					QueryFrom:    req.From,
					QueryTo:      req.To,
					QueryCons:    req.ConsReq,
					Consolidator: req.Consolidator,
				}}, nil}
			}
			wg.Done()
			// pop an item of our limiter so that other requests can be processed.
			reqLimiter.Release()
		}(req)
	}
	go func() {
		wg.Wait()
		close(responses)
	}()
	out := make([]models.Series, 0, len(reqs))
	for resp := range responses {
		if resp.err != nil {
			tags.Error.Set(span, true)
			return nil, resp.err
		}
		out = append(out, resp.series...)
	}

	ss.Trace(span)
	log.Debugf("DP getTargetsLocal: %d series found locally", len(out))
	return out, nil

}

// getTarget returns the series for the request in canonical form.
// as ConsolidateContext just processes what it's been given (not "stable" or bucket-aligned to the output interval)
// we simply make sure to pass it the right input such that the output is canonical.
func (s *Server) getTarget(ctx context.Context, ss *models.StorageStats, req models.Req) (points []schema.Point, interval uint32, err error) {
	defer doRecover(&err)
	readRollup := req.Archive != 0 // do we need to read from a downsampled series?
	normalize := req.AggNum > 1    // do we need to normalize points at runtime?
	// normalize is runtime consolidation but only for the purpose of bringing high-res
	// series to the same resolution of lower res series.

	if normalize {
		log.Debugf("DP getTarget() %s normalize:true", req.DebugString())
	} else {
		log.Debugf("DP getTarget() %s normalize:false", req.DebugString())
	}

	if !readRollup && !normalize {
		fixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.None)
		return fixed, req.OutInterval, err
	} else if !readRollup && normalize {
		fixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.None)
		if err != nil {
			return nil, req.OutInterval, err
		}
		return consolidation.ConsolidateContext(ctx, fixed, req.AggNum, req.Consolidator), req.OutInterval, nil
	} else if readRollup && !normalize {
		if req.Consolidator == consolidation.Avg {
			sumFixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.Sum)
			if err != nil {
				return nil, req.OutInterval, err
			}
			cntFixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.Cnt)
			if err != nil {
				return nil, req.OutInterval, err
			}
			return divideContext(
				ctx,
				sumFixed,
				cntFixed,
			), req.OutInterval, nil
		} else {
			fixed, err := s.getSeriesFixed(ctx, ss, req, req.Consolidator)
			return fixed, req.OutInterval, err
		}
	} else {
		// readRollup && normalize
		if req.Consolidator == consolidation.Avg {
			sumFixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.Sum)
			if err != nil {
				return nil, req.OutInterval, err
			}
			cntFixed, err := s.getSeriesFixed(ctx, ss, req, consolidation.Cnt)
			if err != nil {
				return nil, req.OutInterval, err
			}
			return divideContext(
				ctx,
				consolidation.ConsolidateContext(ctx, sumFixed, req.AggNum, consolidation.Sum),
				consolidation.ConsolidateContext(ctx, cntFixed, req.AggNum, consolidation.Sum),
			), req.OutInterval, nil
		} else {
			fixed, err := s.getSeriesFixed(ctx, ss, req, req.Consolidator)
			if err != nil {
				return nil, req.OutInterval, err
			}
			return consolidation.ConsolidateContext(ctx, fixed, req.AggNum, req.Consolidator), req.OutInterval, nil
		}
	}
}

func logLoad(typ string, key schema.AMKey, from, to uint32) {
	log.Debugf("DP load from %-6s %20s %d - %d (%s - %s) span:%ds", typ, key, from, to, util.TS(from), util.TS(to), to-from-1)
}

// getSeriesFixed fetches the series and returns it in quantized, pre-canonical form.
// TODO: we can probably forego Fix if archive > 0, because only raw chunks are not quantized yet.
// the requested consolidator is the one that will be used for selecting the archive to read from
func (s *Server) getSeriesFixed(ctx context.Context, ss *models.StorageStats, req models.Req, consolidator consolidation.Consolidator) ([]schema.Point, error) {
	select {
	case <-ctx.Done():
		//request canceled
		return nil, nil
	default:
	}
	rctx := newRequestContext(ctx, &req, consolidator)
	// see newRequestContext for a detailed explanation of this.
	if rctx.From == rctx.To {
		return nil, nil
	}
	res, err := s.getSeries(rctx, ss)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		//request canceled
		return nil, nil
	default:
	}
	res.Points = append(s.itersToPoints(rctx, res.Iters), res.Points...) // TODO the output from s.itersToPoints is never released to the pool?
	// note: Fix() returns res.Points back to the pool
	// this is safe because nothing else is still using it
	// you can confirm this by analyzing what happens in prior calls such as itertoPoints and s.getSeries()
	return Fix(res.Points, rctx.From, rctx.To, req.ArchInterval), nil
}

// getSeries returns points from mem (and store if needed), within the range from (inclusive) - to (exclusive)
// it can query for data within aggregated archives, by using fn min/max/sum/cnt and providing the matching agg span as interval
// pass consolidation.None as consolidator to mean read from raw interval, otherwise we'll read from aggregated series.
func (s *Server) getSeries(ctx *requestContext, ss *models.StorageStats) (mdata.Result, error) {
	res, err := s.getSeriesAggMetrics(ctx)
	if err != nil {
		return res, err
	}
	select {
	case <-ctx.ctx.Done():
		//request canceled
		return res, nil
	default:
	}
	ss.IncChunksFromTank(uint32(len(res.Iters)))

	log.Debugf("oldest from aggmetrics is %d", res.Oldest)
	span := opentracing.SpanFromContext(ctx.ctx)

	if res.Oldest <= ctx.From {
		reqSpanMem.ValueUint32(ctx.To - ctx.From)
		return res, nil
	}

	// if oldest < to -> search until oldest, we already have the rest from mem
	// if to < oldest -> no need to search until oldest, only search until to
	// adds iters from both the cache and the store (if applicable)
	until := util.Min(res.Oldest, ctx.To)
	fromCache, err := s.getSeriesCachedStore(ctx, ss, until)
	if err != nil {
		tracing.Failure(span)
		tracing.Error(span, err)
		log.Errorf("getSeriesCachedStore: %s", err.Error())
		return res, err
	}
	res.Iters = append(fromCache, res.Iters...)
	return res, nil
}

// itersToPoints converts the iters to points if they are within the from/to range
// TODO: just work on the result directly
func (s *Server) itersToPoints(ctx *requestContext, iters []tsz.Iter) []schema.Point {
	pre := time.Now()

	points := pointSlicePool.Get().([]schema.Point)
	for _, iter := range iters {
		total := 0
		good := 0
		for iter.Next() {
			total += 1
			ts, val := iter.Values()
			if ts >= ctx.From && ts < ctx.To {
				good += 1
				points = append(points, schema.Point{Val: val, Ts: ts})
			}
		}
		log.Debugf("DP getSeries: iter values good/total %d/%d", good, total)
	}
	itersToPointsDuration.Value(time.Now().Sub(pre))
	return points
}

func (s *Server) getSeriesAggMetrics(ctx *requestContext) (mdata.Result, error) {
	// this is a query node that for some reason received a request
	if s.MemoryStore == nil {
		return mdata.Result{}, nil
	}

	metric, ok := s.MemoryStore.Get(ctx.AMKey.MKey)
	if !ok {
		return mdata.Result{
			Oldest: ctx.Req.To,
		}, nil
	}

	logLoad("memory", ctx.AMKey, ctx.From, ctx.To)
	if ctx.Cons != consolidation.None {
		return metric.GetAggregated(ctx.Cons, ctx.Req.ArchInterval, ctx.From, ctx.To)
	} else {
		return metric.Get(ctx.From, ctx.To)
	}
}

// will only fetch until until, but uses ctx.To for debug logging
func (s *Server) getSeriesCachedStore(ctx *requestContext, ss *models.StorageStats, until uint32) ([]tsz.Iter, error) {

	// this is a query node that for some reason received a data request
	if s.BackendStore == nil {
		return nil, nil
	}

	var iters []tsz.Iter
	var prevts uint32

	reqSpanBoth.ValueUint32(ctx.To - ctx.From)
	logLoad("cassan", ctx.AMKey, ctx.From, ctx.To)

	log.Debugf("cache: searching query key %s, from %d, until %d", ctx.AMKey, ctx.From, until)
	cacheRes, err := s.Cache.Search(ctx.ctx, ctx.AMKey, ctx.From, until)
	if err != nil {
		return iters, fmt.Errorf("Cache.Search() failed: %+v", err.Error())
	}
	log.Debugf("cache: result start %d, end %d", len(cacheRes.Start), len(cacheRes.End))
	ss.IncCacheResult(cacheRes.Type)

	// check to see if the request has been canceled, if so abort now.
	select {
	case <-ctx.ctx.Done():
		//request canceled
		return iters, nil
	default:
	}

	for _, itgen := range cacheRes.Start {
		iter, err := itgen.Get()
		prevts = itgen.T0
		if err != nil {
			// TODO(replay) figure out what to do if one piece is corrupt
			return iters, fmt.Errorf("error getting iter from cacheResult.Start: %+v", err.Error())
		}
		iters = append(iters, iter)
	}
	ss.IncChunksFromCache(uint32(len(cacheRes.Start)))

	// check to see if the request has been canceled, if so abort now.
	select {
	case <-ctx.ctx.Done():
		//request canceled
		return iters, nil
	default:
	}

	// the request cannot completely be served from cache, it will require store involvement
	if cacheRes.Type != cache.Hit {
		if cacheRes.From != cacheRes.Until {
			storeIterGens, err := s.BackendStore.Search(ctx.ctx, ctx.AMKey, ctx.Req.TTL, cacheRes.From, cacheRes.Until)
			if err != nil {
				return iters, fmt.Errorf("BackendStore.Search() failed: %+v", err.Error())
			}
			// check to see if the request has been canceled, if so abort now.
			select {
			case <-ctx.ctx.Done():
				//request canceled
				return iters, nil
			default:
			}

			for i, itgen := range storeIterGens {
				iter, err := itgen.Get()
				if err != nil {
					// TODO(replay) figure out what to do if one piece is corrupt
					if i > 0 {
						// add all the iterators that are in good shape
						s.Cache.AddRange(ctx.AMKey, prevts, storeIterGens[:i])
					}
					return iters, fmt.Errorf("error getting iter from BackendStore.Search(): %+v", err.Error())
				}
				iters = append(iters, iter)
			}
			ss.IncChunksFromStore(uint32(len(storeIterGens)))
			// it's important that the itgens get added in chronological order,
			// currently we rely on store returning results in order
			s.Cache.AddRange(ctx.AMKey, prevts, storeIterGens)
		}

		// the End slice is in reverse order
		for i := len(cacheRes.End) - 1; i >= 0; i-- {
			iter, err := cacheRes.End[i].Get()
			if err != nil {
				// TODO(replay) figure out what to do if one piece is corrupt
				return iters, fmt.Errorf("error getting iter from cacheResult.End: %+v", err.Error())
			}
			iters = append(iters, iter)
		}
		ss.IncChunksFromCache(uint32(len(cacheRes.End)))
	}

	return iters, nil
}

// check for duplicate series names for the same query. If found merge the results.
// each first uniquely-identified series's backing datapoints slice is reused
// any subsequent non-uniquely-identified series is merged into the former and has its
// datapoints slice returned to the pool. input series must be canonical
func mergeSeries(in []models.Series) []models.Series {
	type segment struct {
		target string
		query  string
		from   uint32
		to     uint32
		con    consolidation.Consolidator
	}
	seriesByTarget := make(map[segment][]models.Series)
	for _, series := range in {
		s := segment{
			series.Target,
			series.QueryPatt,
			series.QueryFrom,
			series.QueryTo,
			series.Consolidator,
		}
		seriesByTarget[s] = append(seriesByTarget[s], series)
	}
	merged := make([]models.Series, len(seriesByTarget))
	i := 0
	for _, series := range seriesByTarget {
		if len(series) == 1 {
			merged[i] = series[0]
		} else {
			// we use the first series in the list as our result.  We check over every
			// point and if it is null, we then check the other series for a non null
			// value to use instead.
			log.Debugf("DP mergeSeries: %s has multiple series.", series[0].Target)
			for i := range series[0].Datapoints {
				for j := 0; j < len(series); j++ {
					if !math.IsNaN(series[j].Datapoints[i].Val) {
						series[0].Datapoints[i].Val = series[j].Datapoints[i].Val
						break
					}
				}
			}
			for j := 1; j < len(series); j++ {
				pointSlicePool.Put(series[j].Datapoints[:0])
			}
			merged[i] = series[0]
		}
		i++
	}
	return merged
}

// requestContext is a more concrete specification to load data based on a models.Req
type requestContext struct {
	ctx context.Context

	// request by external user.
	Req *models.Req // note: we don't actually modify Req, so by value would work too

	// internal request needed to fetch and fix the data.
	Cons  consolidation.Consolidator // to satisfy avg request from user, this would be sum or cnt
	From  uint32                     // may be different than user request, see below
	To    uint32                     // may be different than user request, see below
	AMKey schema.AMKey               // set by combining Req's key, consolidator and archive info
}

func newRequestContext(ctx context.Context, req *models.Req, consolidator consolidation.Consolidator) *requestContext {

	rc := requestContext{
		ctx:  ctx,
		Req:  req,
		Cons: consolidator,
	}

	// while aggregated archives are quantized, raw intervals are not.  quantizing happens after fetching the data,
	// So for raw data, we have to adjust the range to get the right data.
	// (ranges described as a..b include both and b)
	// Assuming minutely data:
	// REQ           0[---FROM---60]----------120-----------180[----TO----240]  any request from 1..60 to 181..240 should ...
	// QUANTD RESULT 0----------[60]---------[120]---------[180]                return points 60, 120 and 180 (simply because of to/from and inclusive/exclusive rules) ..
	// STORED DATA   0[----------60][---------120][---------180][---------240]  but data for 60 may be at 1..60, data for 120 at 61..120 and for 180 at 121..180 (due to quantizing)
	// to retrieve the stored data, we also use from inclusive and to exclusive,
	// so to make sure that the data after quantization (Fix()) is correct, we have to make the following adjustment:
	// `from`   1..60 needs data    1..60   -> to assure we can read that data we adjust `from` to previous boundary+1 (here 1). (will be quantized to next boundary in this case 60)
	// `to`  181..240 needs data 121..180   -> to avoid reading needless data  we adjust `to`   to previous boundary+1 (here 181), last ts returned must be 180

	// except... there's a special case. let's say archinterval=60, and user requests:
	// to=25, until=36
	// we know that in the graphite model there will be no data in that timeframe:
	// maybe the user submitted a point with ts=30 but it will be quantized to ts=60 so it is out of range.
	// but wouldn't it be nice to include it anyway?
	// we can argue both ways, but the other thing is that if we apply the logic above, we end up with:
	// from := 1
	// to := 1
	// which is a query not accepted by AggMetric, Ccache or store. (from must be less than to)
	// the only way we can get acceptable queries is for from to be 1 and to to remain 36 (or become 60 or 61)
	// such a fetch request would include the requested point
	// but we know Fix() will later create the output according to these rules:
	// * first point should have the first timestamp >= from that divides by interval (that would be 60 in this case)
	// * last point should have the last timestamp < to that divides by interval (because to is always exclusive) (that would be 0 in this case, unless we change to to 61)
	// which wouldn't make sense of course. one could argue it should output one point with ts=60,
	// but to do that, we have to "broaden" the `to` requested by the user, covering a larger time frame they didn't actually request.
	// and we deviate from the quantizing model.
	// I think we should just stick to the quantizing model

	// we can do the logic above backwards: if from and to are adjusted to the same value, such as 181, it means `from` was 181..240  and `to` was 181..240
	// which is either a nonsensical request (from > to, from == to) or from < to but such that the requested timeframe falls in between two adjacent quantized
	// timestamps and could not include either of them.
	// so the caller can just compare rc.From and rc.To and if equal, immediately return [] to the client.

	if req.Archive == 0 {
		rc.From = alignBackward(req.From, req.ArchInterval) + 1
		rc.To = alignBackward(req.To, req.ArchInterval) + 1
		rc.AMKey = schema.AMKey{MKey: req.MKey}
	} else {
		rc.From = req.From
		rc.To = req.To
		rc.AMKey = schema.GetAMKey(req.MKey, consolidator.Archive(), req.ArchInterval)
	}

	if req.AggNum > 1 {
		// the series needs to be pre-canonical. There's 2 aspects to this

		// 1) `from` adjustment.
		// we may need to rewind the from so that we make sure to include all the necessary input raw data
		// to be able to compute the first aggregated point of the canonical output
		// because getTarget() output must be in canonical form.
		// e.g. with req.ArchInterval = 10, req.OutInterval = 30, req.AggNum = 3
		// points must be 40,50,60 , 70,80,90 such that they consolidate to 60, 90, ...
		// so the fixed output must start at 40, or generally speaking at a point with ts such that ts-archInterval % outInterval = 0.

		// So if the user requested a rc.From value of 55 (for archive=0 we would have adjusted rc.From to 51 per the rules above to make sure
		// to include any point that would correspond to the first fixed value, 60, but that doesn't matter here)
		// our firstPointTs would be 60 here.
		// but because we eventually want to consolidate into a point with ts=60, we also need the points that will be fix-adjusted to 40 and 50.
		// so From needs to be lowered by 20 to become 35 (or 31 if adjusted).

		boundary := alignForward(rc.From, req.OutInterval)
		rewind := req.AggNum * req.ArchInterval
		if boundary < rewind {
			panic(fmt.Sprintf("Cannot rewind far back enough (trying to rewind by %d from timestamp %d)", rewind, boundary))
		}
		rc.From = boundary - rewind + 1

		// 2) `to` adjustment.
		// if the series has some excess at the end, it may aggregate into a bucket with a timestamp out of the desired range.
		// for example: imagine we take the case from above, and the user specified a `to` of 115.
		// a native 30s series would end with point 90. We should not include any points that would go into an aggregation bucket with timestamp higher than 90.
		// (such as 100 or 110 which would technically be allowed by the `to` specification)
		// so the proper to value is the highest value that does not result in points going into an out-of-bounds bucket.

		// example: for 10s data (note that the 2 last colums should always match!)
		// * means it has been adjusted due to querying raw archive, per above
		// rc.To old - rc.To new - will fetch data - will be fixed to - will be aggregated to a bucket with ts - native 30s series will have last point for given rc.To old
		// 91        - 91        - ...,90          - 90               - 90                                     - 90
		// 90        - 61        - ...,60          - 60               - 60                                     - 60
		// 180       - 151       - ...,150         - 150              - 150                                    - 150
		//*180->171  - 151       - ...,150         - 150              - 150                                    - 150
		// 181       - 181       - ...,180         - 180              - 180                                    - 180
		//*181->181  - 180       - ...,180         - 180              - 180                                    - 180
		// 240       - 211       - ...,210         - 210              - 210                                    - 210
		//*240->231  - 211       - ...,210         - 210              - 210                                    - 210

		rc.To = alignBackward(rc.To, req.OutInterval) + 1
	}

	return &rc
}

//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package planner

import (
	"github.com/couchbase/query/algebra"
	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/plan"
	base "github.com/couchbase/query/plannerbase"
	"github.com/couchbase/query/util"
	"github.com/couchbase/query/value"
)

// Covering Scan

func (this *builder) buildCovering(indexes, unnestIndexes, flex map[datastore.Index]*indexEntry,
	node *algebra.KeyspaceTerm, baseKeyspace *base.BaseKeyspace, subset, id expression.Expression,
	searchSargables []*indexEntry, unnests []*algebra.Unnest) (
	scan plan.SecondaryScan, sargLength int, err error) {

	// covering turrned off or ANSI NEST
	if this.cover == nil || node.IsAnsiNest() {
		return
	}

	hasDeltaKeyspace := this.context.HasDeltaKeyspace(baseKeyspace.Keyspace())

	// GSI covering scan
	scan, sargLength, err = this.buildCoveringScan(indexes, node, baseKeyspace, id)
	if scan != nil || err != nil {
		return
	}

	// Delta keyspace present no covering
	if hasDeltaKeyspace {
		return
	}
	// GSI Unnest covering scan
	if len(unnests) > 0 && len(unnestIndexes) > 0 {
		scan, sargLength, err = this.buildCoveringUnnestScan(node, baseKeyspace.DnfPred(),
			subset, id, unnestIndexes, unnests)
		if scan != nil || err != nil {
			return
		}
	}

	// Flex FTS covering scan
	scan, sargLength, err = this.buildFlexSearchCovering(flex, node, baseKeyspace, id)
	if scan != nil || err != nil {
		return
	}

	// FTS SEARCH() covering scan
	return this.buildSearchCovering(searchSargables, node, baseKeyspace, id)
}

func (this *builder) buildCoveringScan(idxs map[datastore.Index]*indexEntry,
	node *algebra.KeyspaceTerm, baseKeyspace *base.BaseKeyspace,
	id expression.Expression) (plan.SecondaryScan, int, error) {

	if this.cover == nil || len(idxs) == 0 {
		return nil, 0, nil
	}

	indexes := idxs
	hasDeltaKeyspace := this.context.HasDeltaKeyspace(baseKeyspace.Keyspace())
	if hasDeltaKeyspace {
		indexes = make(map[datastore.Index]*indexEntry, 1)
		for index, entry := range idxs {
			if index.IsPrimary() {
				indexes[index] = entry
				break
			}
		}
		if len(indexes) == 0 {
			return nil, 0, nil
		}
	}

	alias := node.Alias()
	exprs := this.cover.Expressions()
	pred := baseKeyspace.DnfPred()
	origPred := baseKeyspace.OrigPred()
	useCBO := this.useCBO && this.keyspaceUseCBO(alias)

	narrays := 0
	coveringEntries := _COVERING_ENTRY_POOL.Get()
	defer _COVERING_ENTRY_POOL.Put(coveringEntries)

outer:
	for index, entry := range indexes {
		if !useCBO && entry.arrayKey != nil && narrays < len(coveringEntries) {
			continue
		}

		if !this.hasBuilderFlag(BUILDER_CHK_INDEX_ORDER) {
			// Sarg to set spans
			err := this.sargIndexes(baseKeyspace, node.IsUnderHash(), map[datastore.Index]*indexEntry{index: entry})
			if err != nil {
				return nil, 0, err
			}
		}

		keys := entry.keys

		// Matches execution.spanScan.RunOnce()
		if !index.IsPrimary() {
			keys = append(keys, id)
		}

		// Include filter covers
		coveringExprs, filterCovers, err := indexCoverExpressions(entry, keys, pred, origPred, alias, this.context)
		if err != nil {
			return nil, 0, err
		}

		implicitAny := implicitAnyCover(entry, true, this.context.FeatureControls())

		// Skip non-covering index
		for _, expr := range exprs {
			if !expression.IsCovered(expr, alias, coveringExprs, implicitAny) {
				continue outer
			}
		}

		var implcitIndexProj map[int]bool
		if implicitAny {
			mapAnys, err1 := expression.GatherAny(exprs, entry.arrayKey, false)
			if err1 != nil {
				continue
			}
			ifc := implicitFilterCovers(entry.arrayKey)
			if len(ifc) > 0 {
				if len(filterCovers) == 0 {
					filterCovers = ifc
				} else {
					for c, v := range ifc {
						if _, ok := filterCovers[c]; !ok {
							filterCovers[c] = v
						}
					}
				}
			}

			implcitIndexProj = implicitIndexKeysProj(implicitIndexKeys(entry), mapAnys)
		}

		if entry.arrayKey != nil {
			narrays++
		}

		entry.pushDownProperty = this.indexPushDownProperty(entry, keys, nil, pred, alias, nil,
			false, true, (len(this.baseKeyspaces) == 1), implicitAny)
		coveringEntries[index] = &coveringEntry{idxEntry: entry, filterCovers: filterCovers,
			implcitIndexProj: implcitIndexProj, implicitAny: implicitAny}
	}

	// No covering index available
	if len(coveringEntries) == 0 {
		return nil, 0, nil
	}

	index := this.bestCoveringIndex(useCBO, node, coveringEntries, (narrays < len(coveringEntries)))
	coveringEntry := coveringEntries[index]
	keys := coveringEntry.idxEntry.keys
	var implcitIndexProj map[int]bool
	if coveringEntry.implicitAny {
		keys = implicitIndexKeys(coveringEntry.idxEntry)
		implcitIndexProj = coveringEntry.implcitIndexProj
	}

	// Matches execution.spanScan.RunOnce()
	if !index.IsPrimary() {
		keys = append(keys, id)
	}

	// Include covering expression from index keys
	covers := make(expression.Covers, 0, len(keys))
	for _, key := range keys {
		covers = append(covers, expression.NewCover(key))
	}

	return this.buildCreateCoveringScan(coveringEntry.idxEntry, node, id, pred, exprs, keys, false,
		coveringEntry.idxEntry.arrayKey != nil, coveringEntry.implicitAny, covers,
		coveringEntry.filterCovers, implcitIndexProj)
}

func (this *builder) bestCoveringIndex(useCBO bool, node *algebra.KeyspaceTerm,
	coveringEntries map[datastore.Index]*coveringEntry, noArray bool) (index datastore.Index) {
	if useCBO {
		for _, ce := range coveringEntries {
			entry := ce.idxEntry
			if entry.cost <= 0.0 {
				cost, _, card, size, frCost, e := indexScanCost(entry.index, entry.sargKeys,
					this.context.RequestId(), entry.spans, node.Alias(),
					this.advisorValidate(), this.context)
				if e != nil || (cost <= 0.0 || card <= 0.0 || size <= 0 || frCost <= 0.0) {
					useCBO = false
				} else {
					entry.cardinality, entry.cost, entry.frCost, entry.size = card, cost, frCost, size
				}
			}
		}
	}

	var centry *coveringEntry
	if useCBO {
		for _, ce := range coveringEntries {
			// consider pushdown property before considering cost
			if centry == nil {
				centry = ce
			} else {
				c_pushdown := ce.idxEntry.PushDownProperty()
				i_pushdown := centry.idxEntry.PushDownProperty()
				if (c_pushdown > i_pushdown) ||
					((c_pushdown == i_pushdown) &&
						(ce.idxEntry.cost < centry.idxEntry.cost)) {
					centry = ce
				}
			}
		}
		return centry.idxEntry.index
	}

	// Avoid array indexes if possible
	if noArray {
		for a, ce := range coveringEntries {
			if ce.idxEntry.arrayKey != nil {
				delete(coveringEntries, a)
			}
		}
	}

couter:
	// keep indexes with highest continous sargable indexes
	for sc, _ := range coveringEntries {
		se := coveringEntries[sc].idxEntry
		for tc, _ := range coveringEntries {
			if sc != tc {
				te := coveringEntries[tc].idxEntry
				if be := bestIndexBySargableKeys(se, te, se.nEqCond, te.nEqCond); be != nil {
					if be == te {
						delete(coveringEntries, sc)
						continue couter
					}
					delete(coveringEntries, tc)
				}
			}
		}
	}

	// Keep indexes with max sumKeys
	sumKeys := 0
	for _, ce := range coveringEntries {
		if max := ce.idxEntry.sumKeys + ce.idxEntry.nEqCond; max > sumKeys {
			sumKeys = max
		}
	}

	for c, ce := range coveringEntries {
		if ce.idxEntry.sumKeys+ce.idxEntry.nEqCond < sumKeys {
			delete(coveringEntries, c)
		}
	}

	// Use shortest remaining index
	minLen := 0
	for _, ce := range coveringEntries {
		cLen := len(ce.idxEntry.keys)
		if centry == nil {
			centry = ce
			minLen = cLen
		} else {
			c_pushdown := ce.idxEntry.PushDownProperty()
			i_pushdown := centry.idxEntry.PushDownProperty()
			if (c_pushdown > i_pushdown) ||
				((c_pushdown == i_pushdown) &&
					(cLen < minLen || (cLen == minLen && ce.idxEntry.index.Condition() != nil))) {
				centry = ce
				minLen = cLen
			}
		}
	}
	return centry.idxEntry.index
}

func (this *builder) buildCreateCoveringScan(entry *indexEntry, node *algebra.KeyspaceTerm,
	id, pred expression.Expression, exprs, keys expression.Expressions, unnestScan, arrayIndex, implicitAny bool,
	covers expression.Covers, filterCovers map[*expression.Cover]value.Value,
	idxProj map[int]bool) (plan.SecondaryScan, int, error) {

	sargLength := len(entry.sargKeys)
	useCBO := this.useCBO && this.keyspaceUseCBO(node.Alias())
	baseKeyspace, _ := this.baseKeyspaces[node.Alias()]
	hasDeltaKeyspace := this.context.HasDeltaKeyspace(baseKeyspace.Keyspace())
	countPush := arrayIndex
	array := arrayIndex
	if !unnestScan {
		countPush = !arrayIndex
		array = false
	}

	index := entry.index
	duplicates := entry.spans.CanHaveDuplicates(index, this.context.IndexApiVersion(), pred.MayOverlapSpans(), arrayIndex)
	indexProjection := this.buildIndexProjection(entry, exprs, id, index.IsPrimary() || arrayIndex || duplicates, idxProj)

	// Check and reset pagination pushdows
	indexKeyOrders := this.checkResetPaginations(entry, keys)

	// Build old Aggregates on Index2 only
	scan := this.buildCoveringPushdDownIndexScan2(entry, node, baseKeyspace, pred, indexProjection,
		countPush, array, covers, filterCovers)
	if scan != nil {
		return scan, sargLength, nil
	}

	// Aggregates check and reset
	var indexGroupAggs *plan.IndexGroupAggregates
	if !entry.IsPushDownProperty(_PUSHDOWN_GROUPAGGS) {
		this.resetIndexGroupAggs()
	}

	// build plan for aggregates
	indexGroupAggs, indexProjection = this.buildIndexGroupAggs(entry, keys, false, indexProjection)
	projDistinct := entry.IsPushDownProperty(_PUSHDOWN_DISTINCT)

	cost, cardinality, size, frCost := OPT_COST_NOT_AVAIL, OPT_CARD_NOT_AVAIL, OPT_SIZE_NOT_AVAIL, OPT_COST_NOT_AVAIL
	if useCBO && entry.cost > 0.0 && entry.cardinality > 0.0 && entry.size > 0 && entry.frCost > 0.0 {
		if indexGroupAggs != nil {
			cost, cardinality, size, frCost = getIndexGroupAggsCost(index, indexGroupAggs,
				indexProjection, this.keyspaceNames, entry.cardinality)
		} else {
			cost, cardinality, size, frCost = getIndexProjectionCost(index, indexProjection, entry.cardinality)
		}

		if cost > 0.0 && cardinality > 0.0 && size > 0 && frCost > 0.0 {
			entry.cost += cost
			entry.cardinality = cardinality
			entry.size += size
			entry.frCost += frCost
		}
	}

	arrayKey := entry.arrayKey
	if !implicitAny {
		arrayKey = nil
	}
	// generate filters for covering index scan
	var filter expression.Expression
	if indexGroupAggs == nil {
		var err error
		filter, cost, cardinality, size, frCost, err = this.getIndexFilter(index, node.Alias(), entry.spans,
			arrayKey, covers, filterCovers, entry.cost, entry.cardinality, entry.size, entry.frCost)
		if err != nil {
			return nil, 0, err
		}
		if useCBO {
			entry.cardinality, entry.cost, entry.frCost, entry.size = cardinality, cost, frCost, size
		}
	}

	// build plan for IndexScan
	scan = entry.spans.CreateScan(index, node, this.context.IndexApiVersion(), false, projDistinct,
		pred.MayOverlapSpans(), array, this.offset, this.limit, indexProjection, indexKeyOrders,
		indexGroupAggs, covers, filterCovers, filter, entry.cost, entry.cardinality,
		entry.size, entry.frCost, hasDeltaKeyspace)
	if scan != nil {
		scan.SetImplicitArrayKey(arrayKey)
		if entry.index.Type() != datastore.SYSTEM {
			this.collectIndexKeyspaceNames(baseKeyspace.Keyspace())
		}
		this.coveringScans = append(this.coveringScans, scan)
	}

	return scan, sargLength, nil
}

func (this *builder) checkResetPaginations(entry *indexEntry,
	keys expression.Expressions) (indexKeyOrders plan.IndexKeyOrders) {

	// check order pushdown and reset
	if this.order != nil {
		if entry.IsPushDownProperty(_PUSHDOWN_ORDER) {
			_, indexKeyOrders = this.useIndexOrder(entry, keys)
			this.maxParallelism = 1
		} else {
			this.resetOrderOffsetLimit()
		}
	}

	// check offset push down and convert limit = limit + offset
	if this.offset != nil && !entry.IsPushDownProperty(_PUSHDOWN_OFFSET) {
		this.limit = offsetPlusLimit(this.offset, this.limit)
		this.resetOffset()
	}

	// check limit and reset
	if this.limit != nil && !entry.IsPushDownProperty(_PUSHDOWN_LIMIT) {
		this.resetLimit()
	}
	return
}

func (this *builder) buildCoveringPushdDownIndexScan2(entry *indexEntry, node *algebra.KeyspaceTerm,
	baseKeyspace *base.BaseKeyspace, pred expression.Expression, indexProjection *plan.IndexProjection,
	countPush, array bool, covers expression.Covers, filterCovers map[*expression.Cover]value.Value) plan.SecondaryScan {

	// Aggregates supported pre-Index3
	if (useIndex3API(entry.index, this.context.IndexApiVersion()) &&
		util.IsFeatureEnabled(this.context.FeatureControls(), util.N1QL_GROUPAGG_PUSHDOWN)) || !this.oldAggregates ||
		!entry.IsPushDownProperty(_PUSHDOWN_GROUPAGGS) {
		return nil
	}

	defer func() { this.resetIndexGroupAggs() }()

	var indexKeyOrders plan.IndexKeyOrders

	for _, ag := range this.aggs {
		switch agg := ag.(type) {
		case *algebra.Count:
			if !countPush {
				return nil
			}

			distinct := agg.Distinct()
			op := agg.Operands()[0]
			if !distinct || op.Value() == nil {
				scan := this.buildIndexCountScan(node, entry, pred, distinct, covers, filterCovers)
				this.countScan = scan
				return scan
			}

		case *algebra.Min, *algebra.Max:
			indexKeyOrders = make(plan.IndexKeyOrders, 1)
			if _, ok := agg.(*algebra.Min); ok {
				indexKeyOrders[0] = plan.NewIndexKeyOrders(0, false)
			} else {
				indexKeyOrders[0] = plan.NewIndexKeyOrders(0, true)
			}
		default:
			return nil
		}
	}

	this.maxParallelism = 1
	scan := entry.spans.CreateScan(entry.index, node, this.context.IndexApiVersion(), false, false, pred.MayOverlapSpans(),
		array, nil, expression.ONE_EXPR, indexProjection, indexKeyOrders, nil, covers, filterCovers, nil,
		OPT_COST_NOT_AVAIL, OPT_CARD_NOT_AVAIL, OPT_SIZE_NOT_AVAIL, OPT_COST_NOT_AVAIL, false)
	if scan != nil {
		if entry.index.Type() != datastore.SYSTEM {
			this.collectIndexKeyspaceNames(baseKeyspace.Keyspace())
		}
		this.coveringScans = append(this.coveringScans, scan)
	}

	return scan
}

func mapFilterCovers(fc map[expression.Expression]value.Value) map[*expression.Cover]value.Value {
	if len(fc) == 0 {
		return nil
	}

	rv := make(map[*expression.Cover]value.Value, len(fc))
	for e, v := range fc {
		c := expression.NewCover(e)
		rv[c] = v
	}

	return rv
}

func unFlattenKeys(keys expression.Expressions, arrayKey *expression.All) expression.Expressions {
	if arrayKey == nil || !arrayKey.Flatten() {
		return keys
	}
	rv := make(expression.Expressions, 0, len(keys))
	for _, k := range keys {
		if _, ok := k.(*expression.All); !ok {
			rv = append(rv, k)
		}
	}
	return append(rv, arrayKey)
}

func indexCoverExpressions(entry *indexEntry, keys expression.Expressions,
	pred, origPred expression.Expression, keyspace string, context *PrepareContext) (
	expression.Expressions, map[*expression.Cover]value.Value, error) {

	var filterCovers map[*expression.Cover]value.Value
	exprs := make(expression.Expressions, 0, len(keys))
	exprs = append(exprs, unFlattenKeys(keys, entry.arrayKey)...)
	if entry.cond != nil {
		fc := make(map[expression.Expression]value.Value, 2)
		fc = entry.cond.FilterExpressionCovers(fc)
		fc = entry.origCond.FilterExpressionCovers(fc)
		filterCovers = mapFilterCovers(fc)
	}

	// Allow array indexes to cover ANY predicates
	if pred != nil && entry.exactSpans && implicitAnyCover(entry, false, uint64(0)) {
		covers, err := CoversFor(pred, origPred, keys, context)
		if err != nil {
			return nil, nil, err
		}

		if len(covers) > 0 {
			if len(filterCovers) == 0 {
				filterCovers = covers
			} else {
				for c, v := range covers {
					if _, ok := filterCovers[c]; !ok {
						filterCovers[c] = v
					}
				}
			}
		}
	}

	if len(filterCovers) > 0 {
		for c, _ := range filterCovers {
			exprs = append(exprs, c.Covered())
		}
	}

	return exprs, filterCovers, nil
}

func hasSargableArrayKey(entry *indexEntry) bool {
	if entry.arrayKey != nil {
		for i, k := range entry.sargKeys {
			if _, ok := k.(*expression.All); ok &&
				i < len(entry.skeys) && entry.skeys[i] {
				return true
			}
		}
	}
	return false
}

func hasUnknownsInSargableArrayKey(entry *indexEntry) bool {
	if entry.arrayKey != nil && entry.spans != nil {
		for i, k := range entry.sargKeys {
			if _, ok := k.(*expression.All); ok &&
				i < len(entry.skeys) && entry.skeys[i] && entry.spans.CanProduceUnknowns(i) {
				return true
			}
		}
	}
	return false
}

func implicitFilterCovers(expr expression.Expression) map[*expression.Cover]value.Value {
	var fc map[expression.Expression]value.Value
	for all, ok := expr.(*expression.All); ok; all, ok = expr.(*expression.All) {
		if array, ok := all.Array().(*expression.Array); ok {
			if fc == nil {
				fc = make(map[expression.Expression]value.Value, len(array.Bindings())+1)
			}
			for _, b := range array.Bindings() {
				fc[b.Expression()] = value.TRUE_ARRAY_VALUE
			}
			if array.When() != nil {
				fc = array.When().FilterExpressionCovers(fc)
			}
			expr = array.ValueMapping()
		} else {
			break
		}
	}
	return mapFilterCovers(fc)
}

func implicitIndexKeys(entry *indexEntry) (rv expression.Expressions) {
	keys := entry.keys
	all := entry.arrayKey
	pos := entry.arrayKeyPos
	if all == nil || !all.Flatten() {
		return keys
	}
	rv = make(expression.Expressions, 0, len(keys))
	rv = append(rv, keys[0:pos]...)
	rv = append(rv, all.FlattenKeys().Operands()...)
	rv = append(rv, keys[pos+all.FlattenSize():]...)
	return rv
}

func implicitIndexKeysProj(keys expression.Expressions,
	anys map[expression.Expression]expression.Expression) (rv map[int]bool) {
	rv = make(map[int]bool, len(keys))
	for keyPos, indexKey := range keys {
		for _, expr := range anys {
			if expr.DependsOn(indexKey) {
				rv[keyPos] = true
				break
			}
		}
	}
	return
}

func implicitAnyCover(entry *indexEntry, flatten bool, featControl uint64) bool {
	_, ok := entry.spans.(*IntersectSpans)
	if ok || entry.arrayKey == nil || !hasSargableArrayKey(entry) || hasUnknownsInSargableArrayKey(entry) {
		return false
	}
	enabled := !flatten || (util.IsFeatureEnabled(featControl, util.N1QL_IMPLICIT_ARRAY_COVER) &&
		!bindingExpressionInIndexKeys(entry))
	return enabled && (flatten == entry.arrayKey.Flatten())
}

func bindingExpressionInIndexKeys(entry *indexEntry) bool {
	if entry.arrayKey == nil {
		return false
	}
	array, ok := entry.arrayKey.Array().(*expression.Array)
	if !ok {
		for _, key := range entry.keys {
			if expression.Equivalent(key, entry.arrayKey.Array()) {
				return true
			}
		}
		return false
	}
outer:
	for _, b := range array.Bindings() {
		for _, key := range entry.keys {
			if expression.Equivalent(key, b.Expression()) {
				continue outer
			}
		}
		return false
	}
	return true
}

var _FILTER_COVERS_POOL = value.NewStringValuePool(32)
var _STRING_BOOL_POOL = util.NewStringBoolPool(1024)

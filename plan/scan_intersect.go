//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package plan

import (
	"encoding/json"

	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/expression/parser"
	"github.com/couchbase/query/value"
)

// IntersectScan scans multiple indexes and intersects the results.
type IntersectScan struct {
	readonly
	optEstimate
	scans []SecondaryScan
	limit expression.Expression
}

func NewIntersectScan(limit expression.Expression, cost, cardinality float64,
	size int64, frCost float64, scans ...SecondaryScan) *IntersectScan {
	for _, scan := range scans {
		if scan.Limit() != nil {
			scan.SetLimit(nil)
		}
		scan.SetOffset(nil)
	}

	buf := make([]SecondaryScan, 0, 2*len(scans))
	scans = flattenIntersectScans(scans, buf)

	n := len(scans)
	if n > 64 {
		return NewIntersectScan(
			limit, cost, cardinality, size, frCost,
			NewIntersectScan(nil, cost/2.0, cardinality, size, frCost, scans[0:n/2]...),
			NewIntersectScan(nil, cost/2.0, cardinality, size, frCost, scans[n/2:]...),
		)
	}

	rv := &IntersectScan{
		scans: scans,
		limit: limit,
	}
	setOptEstimate(&rv.optEstimate, cost, cardinality, size, frCost)
	return rv
}

func (this *IntersectScan) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitIntersectScan(this)
}

func (this *IntersectScan) New() Operator {
	return &IntersectScan{}
}

func (this *IntersectScan) Covers() expression.Covers {
	if this.Covering() {
		return this.scans[0].Covers()
	} else {
		return nil
	}
}

func (this *IntersectScan) FilterCovers() map[*expression.Cover]value.Value {
	if this.Covering() {
		return this.scans[0].FilterCovers()
	} else {
		return nil
	}
}

func (this *IntersectScan) Covering() bool {
	for _, scan := range this.scans {
		if !scan.Covering() {
			return false
		}
	}

	return true
}

func (this *IntersectScan) SetCovers(covers expression.Covers) {
}

func (this *IntersectScan) Scans() []SecondaryScan {
	return this.scans
}

func (this *IntersectScan) Limit() expression.Expression {
	return this.limit
}

func (this *IntersectScan) SetLimit(limit expression.Expression) {
	this.limit = limit
}

func (this *IntersectScan) Offset() expression.Expression {
	return nil
}

func (this *IntersectScan) SetOffset(limit expression.Expression) {
}

func (this *IntersectScan) IsUnderNL() bool {
	return this.scans[0].IsUnderNL()
}

func (this *IntersectScan) CoverJoinSpanExpressions(coverer *expression.Coverer,
	implicitArrayKey *expression.All) error {
	for _, scan := range this.scans {
		err := scan.CoverJoinSpanExpressions(coverer, implicitArrayKey)
		if err != nil {
			return err
		}
	}

	return nil
}

func (this *IntersectScan) GroupAggs() *IndexGroupAggregates {
	return nil
}

func (this *IntersectScan) OrderTerms() IndexKeyOrders {
	return nil
}

func (this *IntersectScan) GetIndex() datastore.Index {
	return nil
}

func (this *IntersectScan) String() string {
	bytes, _ := this.MarshalJSON()
	return string(bytes)
}

func (this *IntersectScan) MarshalJSON() ([]byte, error) {
	return json.Marshal(this.MarshalBase(nil))
}

func (this *IntersectScan) MarshalBase(f func(map[string]interface{})) map[string]interface{} {
	r := map[string]interface{}{"#operator": "IntersectScan"}

	if this.limit != nil {
		r["limit"] = expression.NewStringer().Visit(this.limit)
	}

	if optEstimate := marshalOptEstimate(&this.optEstimate); optEstimate != nil {
		r["optimizer_estimates"] = optEstimate
	}

	if f != nil {
		f(r)
	} else {
		r["scans"] = this.scans
	}
	return r
}

func (this *IntersectScan) UnmarshalJSON(body []byte) error {
	var _unmarshalled struct {
		_           string                 `json:"#operator"`
		Scans       []json.RawMessage      `json:"scans"`
		Limit       string                 `json:"limit"`
		OptEstimate map[string]interface{} `json:"optimizer_estimates"`
	}

	err := json.Unmarshal(body, &_unmarshalled)
	if err != nil {
		return err
	}

	this.scans = make([]SecondaryScan, 0, len(_unmarshalled.Scans))

	for _, raw_scan := range _unmarshalled.Scans {
		var scan_type struct {
			Operator string `json:"#operator"`
		}

		err = json.Unmarshal(raw_scan, &scan_type)
		if err != nil {
			return err
		}

		scan_op, err := MakeOperator(scan_type.Operator, raw_scan)
		if err != nil {
			return err
		}

		this.scans = append(this.scans, scan_op.(SecondaryScan))
	}

	if _unmarshalled.Limit != "" {
		this.limit, err = parser.Parse(_unmarshalled.Limit)
		if err != nil {
			return err
		}
	}

	unmarshalOptEstimate(&this.optEstimate, _unmarshalled.OptEstimate)

	return nil
}

func flattenIntersectScans(scans, buf []SecondaryScan) []SecondaryScan {
	for _, scan := range scans {
		switch scan := scan.(type) {
		case *IntersectScan:
			buf = flattenIntersectScans(scan.scans, buf)
		default:
			buf = append(buf, scan)
		}
	}

	return buf
}

func (this *IntersectScan) verify(prepared *Prepared) bool {
	for _, scan := range this.scans {
		if !scan.verify(prepared) {
			return false
		}
	}

	return true
}

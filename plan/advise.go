//  Copyright 2019-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package plan

import (
	"encoding/json"
)

type Advise struct {
	execution
	op    Operator
	query string
}

func NewAdvise(op Operator, text string) *Advise {
	return &Advise{
		op:    op,
		query: text,
	}
}

func (this *Advise) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitAdvise(this)
}

func (this *Advise) New() Operator {
	return &Advise{}
}

func (this *Advise) Operator() Operator {
	return this.op
}

func (this *Advise) MarshalJSON() ([]byte, error) {
	return json.Marshal(this.MarshalBase(nil))
}

func (this *Advise) MarshalBase(f func(map[string]interface{})) map[string]interface{} {
	r := map[string]interface{}{"#operator": "Advise"}

	r["query"] = this.query

	if f != nil {
		f(r)
	} else {
		r["advice"] = this.op
	}
	return r
}

func (this *Advise) UnmarshalJSON(body []byte) error {
	var _unmarshalled struct {
		Op   json.RawMessage `json:"advice"`
		Text string          `json:"query"`
	}

	var op_type struct {
		Operator string `json:"#operator"`
	}

	err := json.Unmarshal(body, &_unmarshalled)
	if err != nil {
		return err
	}

	err = json.Unmarshal(_unmarshalled.Op, &op_type)
	if err != nil {
		return err
	}

	this.query = _unmarshalled.Text

	return nil
}

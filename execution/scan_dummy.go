//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package execution

import (
	"encoding/json"
	_ "fmt"

	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/util"
	"github.com/couchbase/query/value"
)

type DummyScan struct {
	base
	plan *plan.DummyScan
}

var _DUMMYSCAN_OP_POOL util.FastPool

func init() {
	util.NewFastPool(&_DUMMYSCAN_OP_POOL, func() interface{} {
		return &DummyScan{}
	})
}

func NewDummyScan(plan *plan.DummyScan, context *Context) *DummyScan {
	rv := _DUMMYSCAN_OP_POOL.Get().(*DummyScan)
	rv.plan = plan
	newRedirectBase(&rv.base)
	rv.output = rv
	return rv
}

func (this *DummyScan) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitDummyScan(this)
}

func (this *DummyScan) Copy() Operator {
	rv := _DUMMYSCAN_OP_POOL.Get().(*DummyScan)
	rv.plan = this.plan
	this.base.copy(&rv.base)
	return rv
}

func (this *DummyScan) PlanOp() plan.Operator {
	return this.plan
}

func (this *DummyScan) RunOnce(context *Context, parent value.Value) {
	this.once.Do(func() {
		defer context.Recover(&this.base) // Recover from any panic
		active := this.active()
		this.switchPhase(_EXECTIME)
		defer func() {
			this.notify()
			this.switchPhase(_NOTIME)
			this.close(context)
		}()
		if !active {
			return
		}

		av := value.EMPTY_ANNOTATED_OBJECT

		if parent != nil {
			cv := value.NewScopeValue(_EMPTY_OBJECT, parent)
			av = value.NewAnnotatedValue(cv)
		}

		this.sendItem(av)
	})
}

func (this *DummyScan) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

func (this *DummyScan) Done() {
	this.baseDone()
	if this.isComplete() {
		_DUMMYSCAN_OP_POOL.Put(this)
	}
}

var _EMPTY_OBJECT = map[string]interface{}{}

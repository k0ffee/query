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

	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/value"
)

type Unnest struct {
	base
	plan *plan.Unnest
}

func NewUnnest(plan *plan.Unnest, context *Context) *Unnest {
	rv := &Unnest{
		plan: plan,
	}

	newBase(&rv.base, context)
	rv.output = rv
	return rv
}

func (this *Unnest) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitUnnest(this)
}

func (this *Unnest) Copy() Operator {
	rv := &Unnest{plan: this.plan}
	this.base.copy(&rv.base)
	return rv
}

func (this *Unnest) PlanOp() plan.Operator {
	return this.plan
}

func (this *Unnest) RunOnce(context *Context, parent value.Value) {
	this.runConsumer(this, context, parent)
}

func (this *Unnest) processItem(item value.AnnotatedValue, context *Context) bool {
	ev, err := this.plan.Term().Expression().Evaluate(item, context)
	if err != nil {
		context.Error(errors.NewEvaluationError(err, "UNNEST path"))
		return false
	}

	// not an array, treat as outer unnest
	if ev.Type() != value.ARRAY {
		return !this.plan.Term().Outer() || this.sendItem(item)
	}

	idx := 0

	// empty, treat as outer unnest
	act, ok := ev.Index(idx)
	if act.Type() == value.MISSING && !ok {
		return !this.plan.Term().Outer() || this.sendItem(item)
	}

	// Attach and send
	for {
		var av value.AnnotatedValue

		actv := value.NewAnnotatedValue(act)
		actv.SetAttachment("unnest_position", idx)

		idx++
		newAct, ok := ev.Index(idx)

		isEnd := newAct.Type() == value.MISSING && !ok
		if isEnd {
			av = item
		} else {
			av = value.NewAnnotatedValue(item.Copy())
		}
		av.SetField(this.plan.Alias(), actv)

		if this.plan.Filter() != nil {
			result, err := this.plan.Filter().Evaluate(av, context)
			if err != nil {
				context.Error(errors.NewEvaluationError(err, "unnest filter"))
				return false
			}
			if result.Truth() {
				if !this.sendItem(av) {
					return false
				}
			} else {
				av.Recycle()
			}
		} else if !this.sendItem(av) {
			return false
		}

		// no more
		if isEnd {
			break
		}
		act = newAct
	}

	return true
}

func (this *Unnest) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

var _EMPTY_ACTUALS []interface{}

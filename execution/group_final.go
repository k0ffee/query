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
	"fmt"

	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/value"
)

type FinalGroup struct {
	base
	plan   *plan.FinalGroup
	groups map[string]value.AnnotatedValue
}

func NewFinalGroup(plan *plan.FinalGroup, context *Context) *FinalGroup {
	rv := &FinalGroup{
		plan:   plan,
		groups: make(map[string]value.AnnotatedValue),
	}

	newBase(&rv.base, context)
	rv.output = rv
	return rv
}

func (this *FinalGroup) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFinalGroup(this)
}

func (this *FinalGroup) Copy() Operator {
	rv := &FinalGroup{
		plan:   this.plan,
		groups: make(map[string]value.AnnotatedValue),
	}
	this.base.copy(&rv.base)
	return rv
}

func (this *FinalGroup) PlanOp() plan.Operator {
	return this.plan
}

func (this *FinalGroup) RunOnce(context *Context, parent value.Value) {
	this.runConsumer(this, context, parent)
}

func (this *FinalGroup) processItem(item value.AnnotatedValue, context *Context) bool {
	// Generate the group key
	var gk string
	if len(this.plan.Keys()) > 0 {
		var e error
		gk, e = groupKey(item, this.plan.Keys(), context)
		if e != nil {
			context.Fatal(errors.NewEvaluationError(e, "GROUP key"))
			item.Recycle()
			return false
		}
	}

	// Get or seed the group value
	gv := this.groups[gk]
	if gv != nil {
		context.Fatal(errors.NewDuplicateFinalGroupError())
		item.Recycle()
		return false
	}

	gv = item
	this.groups[gk] = gv

	// Compute final aggregates
	aggregates := gv.GetAttachment("aggregates")
	switch aggregates := aggregates.(type) {
	case map[string]value.Value:
		for _, agg := range this.plan.Aggregates() {
			v, e := agg.ComputeFinal(aggregates[agg.String()], context)
			if e != nil {
				context.Fatal(errors.NewGroupUpdateError(
					e, "Error updating final GROUP value."))
				item.Recycle()
				return false
			}

			aggregates[agg.String()] = v
		}

		return true
	default:
		context.Fatal(errors.NewInvalidValueError(fmt.Sprintf(
			"Invalid or missing aggregates of type %T.", aggregates)))
		item.Recycle()
		return false
	}
}

func (this *FinalGroup) afterItems(context *Context) {
	for _, av := range this.groups {
		if !this.sendItem(av) {
			return
		}
	}

	// Mo matching inputs, so send default values
	if len(this.plan.Keys()) == 0 && len(this.groups) == 0 {
		av := value.NewAnnotatedValue(nil)
		aggregates := make(map[string]value.Value, len(this.plan.Aggregates()))
		av.SetAttachment("aggregates", aggregates)
		for _, agg := range this.plan.Aggregates() {
			aggregates[agg.String()], _ = agg.Default(nil, context)
		}

		if context.UseRequestQuota() && context.TrackValueSize(av.Size()) {
			context.Error(errors.NewMemoryQuotaExceededError())
			av.Recycle()
		} else {
			this.sendItem(av)
		}
	}
}

func (this *FinalGroup) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

func (this *FinalGroup) reopen(context *Context) bool {
	rv := this.baseReopen(context)
	this.groups = make(map[string]value.AnnotatedValue)
	return rv
}

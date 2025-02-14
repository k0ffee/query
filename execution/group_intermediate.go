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

// Grouping of groups. Recursable.
type IntermediateGroup struct {
	base
	plan   *plan.IntermediateGroup
	groups map[string]value.AnnotatedValue
}

func NewIntermediateGroup(plan *plan.IntermediateGroup, context *Context) *IntermediateGroup {
	rv := &IntermediateGroup{
		plan:   plan,
		groups: make(map[string]value.AnnotatedValue),
	}

	newBase(&rv.base, context)
	rv.output = rv
	return rv
}

func (this *IntermediateGroup) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitIntermediateGroup(this)
}

func (this *IntermediateGroup) Copy() Operator {
	rv := &IntermediateGroup{
		plan:   this.plan,
		groups: make(map[string]value.AnnotatedValue),
	}
	this.base.copy(&rv.base)
	return rv
}

func (this *IntermediateGroup) PlanOp() plan.Operator {
	return this.plan
}

func (this *IntermediateGroup) RunOnce(context *Context, parent value.Value) {
	this.runConsumer(this, context, parent)
}

func (this *IntermediateGroup) processItem(item value.AnnotatedValue, context *Context) bool {
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
	if gv == nil {

		// avoid recycling of seeding values
		gv = item
		this.groups[gk] = gv
		return true
	}

	// Cumulate aggregates
	part, ok := item.GetAttachment("aggregates").(map[string]value.Value)
	if !ok {
		context.Fatal(errors.NewInvalidValueError(
			fmt.Sprintf("Invalid partial aggregates %v of type %T", part, part)))
		item.Recycle()
		return false
	}

	if context.UseRequestQuota() {
		context.ReleaseValueSize(item.Size())
	}
	item.Recycle()

	cumulative := gv.GetAttachment("aggregates").(map[string]value.Value)
	if !ok {
		context.Fatal(errors.NewInvalidValueError(
			fmt.Sprintf("Invalid cumulative aggregates %v of type %T", cumulative, cumulative)))
		return false
	}

	for _, agg := range this.plan.Aggregates() {
		a := agg.String()
		v, e := agg.CumulateIntermediate(part[a], cumulative[a], context)
		if e != nil {
			context.Fatal(errors.NewGroupUpdateError(
				e, "Error updating intermediate GROUP value."))
			return false
		}

		cumulative[a] = v
	}

	return true
}

func (this *IntermediateGroup) afterItems(context *Context) {
	for _, av := range this.groups {
		if !this.sendItem(av) {
			return
		}
	}
}

func (this *IntermediateGroup) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

func (this *IntermediateGroup) reopen(context *Context) bool {
	rv := this.baseReopen(context)
	this.groups = make(map[string]value.AnnotatedValue)
	return rv
}

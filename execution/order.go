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

	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/sort"
	"github.com/couchbase/query/value"
)

type orderTerm struct {
	term       string
	descending bool
	nullsLast  bool
}

type Order struct {
	base
	plan    *plan.Order
	values  value.AnnotatedValues
	context *Context
	terms   []orderTerm
}

const _ORDER_CAP = 1024

var _ORDER_POOL = value.NewAnnotatedPool(_ORDER_CAP)

func NewOrder(plan *plan.Order, context *Context) *Order {
	rv := &Order{
		plan:   plan,
		values: _ORDER_POOL.Get(),
	}

	newBase(&rv.base, context)
	rv.execPhase = SORT
	rv.output = rv
	return rv
}

func (this *Order) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitOrder(this)
}

func (this *Order) Copy() Operator {
	rv := &Order{
		plan:   this.plan,
		values: _ORDER_POOL.Get(),
	}
	this.base.copy(&rv.base)
	return rv
}

func (this *Order) PlanOp() plan.Operator {
	return this.plan
}

func (this *Order) RunOnce(context *Context, parent value.Value) {
	defer this.releaseValues()
	this.runConsumer(this, context, parent)
}

func (this *Order) processItem(item value.AnnotatedValue, context *Context) bool {
	if len(this.values) == cap(this.values) {
		values := make(value.AnnotatedValues, len(this.values), len(this.values)<<1)
		copy(values, this.values)
		this.releaseValues()
		this.values = values
	}

	this.values = append(this.values, item)
	return true
}

func (this *Order) setupTerms(context *Context) {
	this.context = context
	this.terms = make([]orderTerm, len(this.plan.Terms()))
	for i, term := range this.plan.Terms() {
		this.terms[i].term = term.Expression().String()
		this.terms[i].descending = term.Descending(this.context)
		this.terms[i].nullsLast = term.NullsLast(this.context)
	}
}

func (this *Order) afterItems(context *Context) {
	defer this.releaseValues()
	defer func() {
		this.context = nil
		this.terms = nil
	}()

	// MB-25901 don't sort if we have been stopped
	if this.stopped {
		return
	}

	this.setupTerms(context)
	sort.Sort(this)

	context.SetSortCount(uint64(this.Len()))
	context.AddPhaseCount(SORT, uint64(this.Len()))

	for _, av := range this.values {
		if !this.sendItem(av) {
			return
		}
	}
}

func (this *Order) releaseValues() {
	_ORDER_POOL.Put(this.values)
	this.values = nil
}

func (this *Order) Len() int {
	return len(this.values)
}

func (this *Order) Less(i, j int) bool {
	return this.lessThan(this.values[i], this.values[j])
}

func (this *Order) lessThan(v1 value.AnnotatedValue, v2 value.AnnotatedValue) bool {
	var ev1, ev2 value.Value
	var c int
	var e error

	for i, term := range this.plan.Terms() {
		s := this.terms[i].term

		ev1, e = getOriginalCachedValue(v1, term.Expression(), s, this.context)
		if e != nil {
			return false
		}

		ev2, e = getOriginalCachedValue(v2, term.Expression(), s, this.context)
		if e != nil {
			return false
		}

		if (this.terms[i].descending && this.terms[i].nullsLast) ||
			(!this.terms[i].descending && !this.terms[i].nullsLast) ||
			((ev1.Type() <= value.NULL && ev2.Type() <= value.NULL) ||
				(ev1.Type() > value.NULL && ev2.Type() > value.NULL)) {
			c = ev1.Collate(ev2)
		} else {
			if ev1.Type() <= value.NULL && ev2.Type() > value.NULL {
				c = 1
			} else {
				c = -1
			}
		}

		if c == 0 {
			continue
		} else if this.terms[i].descending {
			return c > 0
		} else {
			return c < 0
		}
	}

	return false
}

func (this *Order) Swap(i, j int) {
	this.values[i], this.values[j] = this.values[j], this.values[i]
}

func (this *Order) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

func (this *Order) reopen(context *Context) bool {
	rv := this.baseReopen(context)
	this.values = _ORDER_POOL.Get()
	return rv
}

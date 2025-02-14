//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package expression

import (
	"math"

	"github.com/couchbase/query/value"
)

/*
Nested expressions are used to access elements inside of arrays.
They support using the bracket notation ([position]) to access
elements inside an array.
*/
type Element struct {
	BinaryFunctionBase
}

func NewElement(first, second Expression) *Element {
	rv := &Element{
		*NewBinaryFunctionBase("element", first, second),
	}

	rv.expr = rv
	return rv
}

/*
Visitor pattern.
*/
func (this *Element) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitElement(this)
}

func (this *Element) Type() value.Type { return value.JSON }

func (this *Element) Evaluate(item value.Value, context Context) (value.Value, error) {
	first, err := this.operands[0].Evaluate(item, context)
	if err != nil {
		return nil, err
	}
	second, err := this.operands[1].Evaluate(item, context)
	if err != nil {
		return nil, err
	}
	switch second.Type() {
	case value.NUMBER:
		s := second.Actual().(float64)
		if s == math.Trunc(s) {
			v, _ := first.Index(int(s))
			return v, nil
		}
	case value.MISSING:
		return value.MISSING_VALUE, nil
	}

	if first.Type() == value.MISSING {
		return value.MISSING_VALUE, nil
	} else {
		return value.NULL_VALUE, nil
	}
}

/*
Factory method pattern.
*/
func (this *Element) Constructor() FunctionConstructor {
	return func(operands ...Expression) Function {
		return NewElement(operands[0], operands[1])
	}
}

func (this *Element) Set(item, val value.Value, context Context) bool {
	second, er := this.Second().Evaluate(item, context)
	if er != nil {
		return false
	}

	first, er := this.First().Evaluate(item, context)
	if er != nil {
		return false
	}

	switch second.Type() {
	case value.NUMBER:
		s := second.Actual().(float64)
		if s == math.Trunc(s) {
			er := first.SetIndex(int(s), val)
			return er == nil
		}
	}

	return false
}

/*
Return false.
*/
func (this *Element) Unset(item value.Value, context Context) bool {
	return false
}

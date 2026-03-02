// Copyright 2026 The Taipan Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package py

// ExtFunction represents a host-provided function that should be resolved
// outside the VM.
type ExtFunction struct {
	Name string
}

var ExtFunctionType = NewType("ext_function", "A host-provided external function")

// NewExtFunction creates a callable placeholder for a host-resolved function.
func NewExtFunction(name string) *ExtFunction {
	return &ExtFunction{Name: name}
}

// Type of this object.
func (o *ExtFunction) Type() *Type {
	return ExtFunctionType
}

// M__call__ should never be reached in normal Taipan execution because the VM
// yields when it sees ExtFunction calls.
func (o *ExtFunction) M__call__(args Tuple, kwargs StringDict) (Object, error) {
	return nil, ExceptionNewf(RuntimeError, "external function %q must be resolved by host", o.Name)
}

var _ Object = (*ExtFunction)(nil)
var _ I__call__ = (*ExtFunction)(nil)

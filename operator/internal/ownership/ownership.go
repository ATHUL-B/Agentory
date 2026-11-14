/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ownership creates and updates objects on behalf of an owner without
// ever taking over objects someone else created.
package ownership

import (
	"context"
	"errors"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ErrNotOwned means an object with the needed name exists and is not
// controlled by the owner. Such objects are never adopted or modified.
var ErrNotOwned = errors.New("exists and is not managed by this Agent")

// Ensure creates or updates obj, controlled by owner, with labels merged in and
// mutate applied. It returns an error wrapping ErrNotOwned, and leaves the
// object untouched, if an object with that name exists without owner as its
// controller.
func Ensure(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object,
	obj client.Object, labels map[string]string, mutate func()) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("unexpected object type %T", obj)
	}
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !metav1.IsControlledBy(existing, owner):
		return fmt.Errorf("%s %s/%s %w", kindOf(obj), obj.GetNamespace(), obj.GetName(), ErrNotOwned)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, obj, func() error {
		obj.SetLabels(MergeLabels(obj.GetLabels(), labels))
		mutate()
		return controllerutil.SetControllerReference(owner, obj, scheme)
	})
	return err
}

// Delete removes obj if it exists and is controlled by owner; anything else is left alone.
func Delete(ctx context.Context, c client.Client, owner client.Object, obj client.Object) error {
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !metav1.IsControlledBy(obj, owner):
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, obj))
}

// MergeLabels returns existing with add applied on top.
func MergeLabels(existing, add map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(add))
	maps.Copy(out, existing)
	maps.Copy(out, add)
	return out
}

func kindOf(obj client.Object) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	return fmt.Sprintf("%T", obj)
}

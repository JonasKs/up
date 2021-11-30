// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"context"
	"os"

	"github.com/crossplane/crossplane/apis/pkg/v1beta1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/validate"

	"github.com/upbound/up/internal/xpkg/dep/cache"
	"github.com/upbound/up/internal/xpkg/dep/marshaler/xpkg"
	"github.com/upbound/up/internal/xpkg/dep/resolver/image"
)

// Manager defines a dependency Manager
type Manager struct {
	c Cache
	i ImageResolver
	x XpkgMarshaler

	acc []*xpkg.ParsedPackage
}

// New returns a new Manager
func New(opts ...Option) (*Manager, error) {
	m := &Manager{}

	c, err := cache.NewLocal()
	if err != nil {
		return nil, err
	}

	x, err := xpkg.NewMarshaler()
	if err != nil {
		return nil, err
	}

	m.i = image.NewResolver()
	m.c = c
	m.x = x
	m.acc = make([]*xpkg.ParsedPackage, 0)

	for _, o := range opts {
		o(m)
	}

	return m, nil
}

// Option modifies the Manager.
type Option func(*Manager)

// WithCache sets the supplied cache.Local on the Manager.
func WithCache(c Cache) Option {
	return func(m *Manager) {
		m.c = c
	}
}

// WithResolver sets the supplied dep.Resolver on the Manager.
func WithResolver(r ImageResolver) Option {
	return func(m *Manager) {
		m.i = r
	}
}

// Snapshot returns a Snapshot containing a view of all of the validators for
// dependencies (both defined and transitive) related to the given slice of
// v1beta1.Dependency.
func (m *Manager) Snapshot(ctx context.Context, deps []v1beta1.Dependency) (*Snapshot, error) {
	view := make(map[schema.GroupVersionKind]*validate.SchemaValidator)

	for _, d := range deps {
		_, acc, err := m.Resolve(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, p := range acc {
			for k, v := range p.Validators() {
				view[k] = v
			}
		}
	}

	return &Snapshot{
		view: view,
	}, nil
}

// Resolve resolves the given package as well as it's transitive dependencies.
// If storage is successful, the resolved dependency is returned, errors
// otherwise.
func (m *Manager) Resolve(ctx context.Context, d v1beta1.Dependency) (v1beta1.Dependency, []*xpkg.ParsedPackage, error) {
	ud := v1beta1.Dependency{}

	e, err := m.retrievePkg(ctx, d)
	if err != nil {
		return ud, m.acc, nil
	}
	m.acc = append(m.acc, e)

	// recursively resolve all transitive dependencies
	// currently assumes we have something from
	if err := m.resolveAllDeps(ctx, e); err != nil {
		return ud, m.acc, err
	}

	ud.Type = e.Type()
	ud.Package = d.Package
	ud.Constraints = e.Version()

	return ud, m.acc, nil
}

// resolveAllDeps recursively resolves the transitive dependencies for a
// given Entry. In addition, resolveAllDeps takes an accumulator for gathering
// the related xpkg.ParsedPackages for the dependency tree.
func (m *Manager) resolveAllDeps(ctx context.Context, p *xpkg.ParsedPackage) error {

	if len(p.Dependencies()) == 0 {
		// no remaining dependencies to resolve
		return nil
	}

	for _, d := range p.Dependencies() {
		e, err := m.retrievePkg(ctx, d)
		if err != nil {
			return err
		}
		m.acc = append(m.acc, e)

		if err := m.resolveAllDeps(ctx, e); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) addPkg(ctx context.Context, d v1beta1.Dependency) (*xpkg.ParsedPackage, error) {
	// this is expensive
	t, i, err := m.i.ResolveImage(ctx, d)
	if err != nil {
		return nil, err
	}

	p, err := m.x.FromImage(d.Package, t, i)
	if err != nil {
		return nil, err
	}

	// add xpkg to cache
	err = m.c.Store(d, p)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func (m *Manager) retrievePkg(ctx context.Context, d v1beta1.Dependency) (*xpkg.ParsedPackage, error) {
	// resolve version prior to Get
	if err := m.finalizeDepVersion(ctx, &d); err != nil {
		return nil, err
	}

	p, err := m.c.Get(d)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if os.IsNotExist(err) {
		// root dependency does not yet exist in cache, store it
		p, err = m.addPkg(ctx, d)
		if err != nil {
			return nil, err
		}
	} else {
		// check if digest is different from what we have locally
		digest, err := m.i.ResolveDigest(ctx, d)
		if err != nil {
			return nil, err
		}

		if p.Digest() != digest {
			// digest is different, update what we have
			p, err = m.addPkg(ctx, d)
			if err != nil {
				return nil, err
			}
		}
	}

	return p, nil
}

// finalizeDepVersion sets the resolved tag version on the supplied v1beta1.Dependency.
func (m *Manager) finalizeDepVersion(ctx context.Context, d *v1beta1.Dependency) error {
	// determine the version (using resolver) to use based on the supplied constraints
	v, err := m.i.ResolveTag(ctx, *d)
	if err != nil {
		return err
	}

	d.Constraints = v
	return nil
}

// Snapshot --
type Snapshot struct {
	view map[schema.GroupVersionKind]*validate.SchemaValidator
}

// View returns the Snapshot's View.
func (s *Snapshot) View() map[schema.GroupVersionKind]*validate.SchemaValidator {
	return s.view
}

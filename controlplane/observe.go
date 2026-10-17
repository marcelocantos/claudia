// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"sync"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// Plane is the Claudia-owned file the seat pump publishes into.
// Jevons replicates it. Jevons does not write these tables.
type Plane struct {
	mu sync.Mutex
	db *sqlpipe.Database
}

// Open creates the Claudia-owned file and its usage and receipt tables.
func Open(path string) (*Plane, error) {
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		return nil, err
	}
	if err := EnsureSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Plane{db: db}, nil
}

// Close releases the file.
func (p *Plane) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return nil
	}
	err := p.db.Close()
	p.db = nil
	return err
}

// PublishUsage upserts one fact. The seat pump calls this.
func (p *Plane) PublishUsage(fact *claudia.UsageFact) error {
	if p == nil || fact == nil {
		return fmt.Errorf("control plane: nil usage fact")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return fmt.Errorf("control plane: closed")
	}
	return PublishUsageFacts(p.db, []claudia.UsageFact{*fact})
}

// PublishReceipt upserts one receipt. The seat pump calls this.
func (p *Plane) PublishReceipt(receipt *claudia.RequestReceipt) error {
	if p == nil || receipt == nil {
		return fmt.Errorf("control plane: nil receipt")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return fmt.Errorf("control plane: closed")
	}
	return PublishReceipt(p.db, receipt)
}

// WithDB runs fn while publishes wait. Replication handshakes use it so a
// publish cannot interleave with peer setup on this file.
func (p *Plane) WithDB(fn func(*sqlpipe.Database) error) error {
	if p == nil || fn == nil {
		return fmt.Errorf("control plane: nil plane")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return fmt.Errorf("control plane: closed")
	}
	return fn(p.db)
}

// Bind makes this plane the publisher the seat pump writes.
// A nil plane clears the publisher.
func Bind(p *Plane) {
	if p == nil {
		claudia.SetControlPlaneObservers(nil, nil)
		return
	}
	claudia.SetControlPlaneObservers(p.PublishUsage, p.PublishReceipt)
}

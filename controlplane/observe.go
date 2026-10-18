// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"os"
	"sync"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
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

// PublishFile opens path when the file already exists and binds it as the
// seat-pump publisher. A missing file is left missing. The returned function
// unbinds the pump and closes the plane. The broker holds that function for
// the process lifetime.
func PublishFile(path string) (func() error, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	plane, err := Open(path)
	if err != nil {
		return nil, err
	}
	Bind(plane)
	// The serving sidecar can die while this process stays up (a kill,
	// a crash). Its Wait path is the only observer that still runs.
	// Broker shutdown below still settles anything that exit missed.
	omp.SetServingExitHook(func(int) {
		_ = plane.WithDB(func(db *sqlpipe.Database) error {
			return SettleStartedObligations(db, SidecarExitReceiptReason)
		})
	})
	return func() error {
		omp.SetServingExitHook(nil)
		// A receipt or usage fact left started has no later terminal
		// fact once this process is gone. Settle those rows before
		// unbinding so the file a later snapshot reads is not an open
		// obligation. Accepted receipts stay accepted.
		err := plane.WithDB(func(db *sqlpipe.Database) error {
			return SettleStartedObligations(db, ShutdownReceiptReason)
		})
		Bind(nil)
		if cerr := plane.Close(); err == nil {
			err = cerr
		}
		return err
	}, nil
}

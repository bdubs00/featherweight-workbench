package sources

// This file is the redrive of a source (docs/ARCHITECTURE.md section
// 13.9). Without it, a claim is done once its run request is on the queue,
// so a job that the kernel sends to the dead letter blocks its key for the
// whole window, and the next delivery of the event is a duplicate. With
// redrive, each claim holds a generation, a random token that the run
// request carries in the metadata workbench.sourceGeneration. Every replica
// reads the dead letter of the queue of the source through one shared
// durable consumer. A dead-letter record that matches the claim, by its
// source, key, generation, agent, and principal, marks the claim failed.
// After the cooldown, the next authenticated delivery of the event takes
// the failed claim again with a new generation and the next attempt,
// until the attempts of the source are used up.
//
// Redrive never deletes a claim, so the generation, the attempts, and the
// time of the failure stay through a failed publish, a duplicate record, a
// race of two replicas, and a restart.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The states of a claim that only a source with redrive writes.
const (
	stateFailed   = "failed"   // the dead letter has the job of the generation
	stateReleased = "released" // the publish failed; the next delivery runs the same generation
)

// revisionConflict reports whether a write lost to another writer of the
// key.
func revisionConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists)
}

// takeRedrive is Take for a source with redrive. A new key gets the first
// attempt. A failed claim gets a new generation and the next attempt after
// the cooldown, while attempts are left. A released claim and a stale
// pending claim keep their generation and attempt, because their request
// can be on the queue already, and the same idempotency key lets a worker
// return the first run.
func (n *natsStore) takeRedrive(ctx context.Context, source, key string, policy Redrive) (ticket, error) {
	if !n.nc.IsConnected() {
		return ticket{}, fmt.Errorf("%w: not connected to NATS", ErrStore)
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	id := keyID(key)
	for range 5 {
		kv, err := n.bucket(ctx, source)
		if err != nil {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
		e, err := kv.Get(ctx, id)
		var c claim
		var rev uint64
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyDeleted):
			c = claim{Key: key, Generation: rand.Text(), Attempt: 1}
		case lost(err):
			n.forget(source)
			continue
		case err != nil:
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		default:
			if json.Unmarshal(e.Value(), &c) != nil || c.Key != key {
				// A value that does not read counts as done, as in Take.
				return ticket{}, ErrDuplicate
			}
			rev = e.Revision()
			if c.Generation == "" || c.Attempt < 1 {
				// A claim from before redrive matches no dead-letter
				// record, so only a stale pending one goes on, as the
				// first attempt with the idempotency key of Take.
				if c.State != statePending {
					return ticket{}, ErrDuplicate
				}
				if n.now().Sub(c.At) < n.stale {
					return ticket{}, ErrInFlight
				}
				c = claim{Key: key, Generation: rand.Text(), Attempt: 1}
				break
			}
			switch c.State {
			case stateFailed:
				if c.Attempt >= policy.MaxAttempts || n.now().Sub(c.FailedAt) < policy.Cooldown {
					return ticket{}, ErrDuplicate
				}
				c.Generation, c.Attempt, c.FailedAt = rand.Text(), c.Attempt+1, time.Time{}
			case stateReleased:
			case statePending:
				if n.now().Sub(c.At) < n.stale {
					return ticket{}, ErrInFlight
				}
			default:
				return ticket{}, ErrDuplicate
			}
		}
		c.State, c.At, c.By = statePending, n.now().UTC(), n.by()
		val, _ := json.Marshal(c)
		if rev == 0 {
			rev, err = kv.Create(ctx, id, val)
		} else {
			rev, err = kv.Update(ctx, id, val, rev)
		}
		if err == nil {
			return ticket{source: source, key: key, id: id, rev: rev, generation: c.Generation, attempt: c.Attempt}, nil
		}
		if !revisionConflict(err) {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
	}
	return ticket{}, ErrInFlight
}

// endRedrive is Finish and Release for a source with redrive. It writes
// the state only while the claim is the pending claim of the ticket, so it
// never overwrites a failure that the dead letter recorded before Finish,
// and it never ends a claim that another replica took over.
func (n *natsStore) endRedrive(ctx context.Context, t ticket, state string) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	kv, err := n.bucket(ctx, t.source)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
	for range 5 {
		e, err := kv.Get(ctx, t.id)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrStore, err)
		}
		var c claim
		if err := json.Unmarshal(e.Value(), &c); err != nil {
			return fmt.Errorf("%w: the claim does not read", ErrStore)
		}
		if c.Generation != t.generation || c.State != statePending || e.Revision() != t.rev {
			return nil
		}
		c.State, c.At = state, n.now().UTC()
		val, _ := json.Marshal(c)
		_, err = kv.Update(ctx, t.id, val, e.Revision())
		if err == nil {
			return nil
		}
		if !revisionConflict(err) {
			return fmt.Errorf("%w: %w", ErrStore, err)
		}
	}
	return fmt.Errorf("%w: the claim kept changing", ErrStore)
}

// recordFailure reads one record of the dead letter of the queue of src
// and marks the matching claim failed. It returns nil when the consumer
// can acknowledge the record: the claim is failed now, or the record
// belongs to no claim of this source. A record of another source, of a
// person who used enqueue, of an older generation, or of a claim that is
// failed already changes nothing, so a duplicate record never moves the
// cooldown. An error means that the store did not answer, and the record
// comes back.
func (n *natsStore) recordFailure(ctx context.Context, src *source, data []byte) error {
	var ev fw.Event
	var dead fw.IntakeDeadData
	if json.Unmarshal(data, &ev) != nil || ev.Type != fw.EvIntakeDead || json.Unmarshal(ev.Data, &dead) != nil {
		n.skipMalformed(src.cfg.Name)
		return nil
	}
	var req fw.RunRequest
	if dead.Queue != src.queue || (dead.Outcome != fw.DeadFailed && dead.Outcome != fw.DeadRejected) ||
		json.Unmarshal(dead.Request, &req) != nil {
		return nil
	}
	md := req.Metadata
	if md[MetaSource] != src.cfg.Name || md[MetaKey] == "" || md[MetaGeneration] == "" ||
		req.Agent != src.cfg.Agent || req.Principal != src.cfg.RunAs || req.PrincipalKind != src.cfg.RunAsKind {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	kv, err := n.bucket(ctx, src.cfg.Name)
	if err != nil {
		return err
	}
	for range 5 {
		e, err := kv.Get(ctx, keyID(md[MetaKey]))
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			// The window of the key ended, so the next delivery runs anyway.
			return nil
		}
		if lost(err) {
			n.forget(src.cfg.Name)
		}
		if err != nil {
			return err
		}
		var c claim
		if json.Unmarshal(e.Value(), &c) != nil || c.Key != md[MetaKey] || c.Generation != md[MetaGeneration] ||
			(c.State != statePending && c.State != stateDone && c.State != stateReleased) {
			return nil
		}
		now := n.now().UTC()
		c.State, c.FailedAt, c.At = stateFailed, now, now
		val, _ := json.Marshal(c)
		_, err = kv.Update(ctx, e.Key(), val, e.Revision())
		if err == nil {
			return nil
		}
		if !revisionConflict(err) {
			return err
		}
	}
	return errors.New("sources: the claim of a dead-letter record kept changing")
}

// skipMalformed logs a record of the dead letter that does not read, at
// most once a minute for the store. The line names the source only: the
// record can hold the text of a ticket, a person, or an error.
func (n *natsStore) skipMalformed(source string) {
	now := time.Now()
	n.mu.Lock()
	log := now.After(n.nextSkipLog)
	if log {
		n.nextSkipLog = now.Add(time.Minute)
	}
	n.mu.Unlock()
	if log {
		n.log.Warn("sources: a record of the dead letter does not read; the redrive skips it", "source", source)
	}
}

// redriveDurable is the name of the consumer of the dead letter of a
// source. Every replica of the front with the same source and queue shares
// it, so one replica handles each record, and a restart goes on where the
// consumer stopped. The name is not durableName of the kernel bus, so the
// consumer of a worker intake never takes these records.
func redriveDurable(src *source) string {
	return "sources_redrive_" + keyID(src.cfg.Name+"\x00"+src.queue)
}

// consumeOutcomes reads the dead letter of the queue of a source with
// redrive until the sources close. A record is acknowledged only after its
// claim is written, so a store that does not answer gets the record again
// after a second.
func (s *Sources) consumeOutcomes(src *source) {
	defer s.wg.Done()
	n, ok := s.store.(*natsStore)
	if !ok {
		// Load refuses redrive on any bus but nats.
		return
	}
	var consumer jetstream.Consumer
	for s.ctx.Err() == nil {
		if consumer == nil {
			ctx, cancel := context.WithTimeout(s.ctx, storeTimeout)
			var err error
			consumer, err = n.js.CreateOrUpdateConsumer(ctx, n.stream, jetstream.ConsumerConfig{
				Durable:       redriveDurable(src),
				Description:   "The dead letter of the source " + src.cfg.Name + " of fw-workbench, for its redrive",
				FilterSubject: fw.DeadLetterOf(src.queue),
				AckPolicy:     jetstream.AckExplicitPolicy,
				AckWait:       30 * time.Second,
				MaxDeliver:    -1,
			})
			cancel()
			if err != nil {
				// The stream is missing until the first worker starts,
				// and NATS can be out of reach.
				s.log.Debug("sources: the consumer of the dead letter is not ready", "source", src.cfg.Name, "error", redact(err))
				if !s.waitOutcomeRetry() {
					return
				}
				continue
			}
		}
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
		if err == nil {
			for msg := range batch.Messages() {
				if s.ctx.Err() != nil {
					return
				}
				if err := n.recordFailure(s.ctx, src, msg.Data()); err != nil {
					s.log.Debug("sources: a record of the dead letter waits for the store", "source", src.cfg.Name)
					_ = msg.NakWithDelay(time.Second)
				} else {
					_ = msg.Ack()
				}
			}
			err = batch.Error()
		}
		if err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			consumer = nil
			if !s.waitOutcomeRetry() {
				return
			}
		}
	}
}

// waitOutcomeRetry waits a second, and reports false when the sources
// close first.
func (s *Sources) waitOutcomeRetry() bool {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

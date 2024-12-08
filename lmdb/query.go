package lmdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"slices"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/internal"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

func (b *LMDBBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)

	if filter.Search != "" {
		close(ch)
		return ch, nil
	}

	// max number of events we'll return
	maxLimit := b.MaxLimit
	var limit int
	if eventstore.IsNegentropySession(ctx) {
		maxLimit = b.MaxLimitNegentropy
		limit = maxLimit
	} else {
		limit = maxLimit / 4
	}
	if filter.Limit > 0 && filter.Limit <= maxLimit {
		limit = filter.Limit
	}
	if tlimit := nostr.GetTheoreticalLimit(filter); tlimit == 0 {
		close(ch)
		return ch, nil
	} else if tlimit > 0 {
		limit = tlimit
	}

	go b.lmdbEnv.View(func(txn *lmdb.Txn) error {
		txn.RawRead = true
		defer close(ch)
		results, err := b.query(txn, filter, limit)

		for _, ie := range results {
			ch <- ie.Event
		}

		return err
	})

	return ch, nil
}

func (b *LMDBBackend) query(txn *lmdb.Txn, filter nostr.Filter, limit int) ([]internal.IterEvent, error) {
	queries, extraAuthors, extraKinds, extraTagKey, extraTagValues, since, err := b.prepareQueries(filter)
	if err != nil {
		return nil, err
	}

	iterators := make([]*iterator, len(queries))
	exhausted := make([]bool, len(queries)) // indicates that a query won't be used anymore
	results := make([][]internal.IterEvent, len(queries))
	pulledPerQuery := make([]int, len(queries))

	// these are kept updated so we never pull from the iterator that is at further distance
	// (i.e. the one that has the oldest event among all)
	// we will continue to pull from it as soon as some other iterator takes the position
	oldest := internal.IterEvent{Q: -1}

	secondPhase := false // after we have gathered enough events we will change the way we iterate
	secondBatch := make([][]internal.IterEvent, 0, len(queries)+1)
	secondPhaseParticipants := make([]int, 0, len(queries)+1)

	// while merging results in the second phase we will alternate between these two lists
	//   to avoid having to create new lists all the time
	var secondPhaseResultsA []internal.IterEvent
	var secondPhaseResultsB []internal.IterEvent
	var secondPhaseResultsToggle bool // this is just a dummy thing we use to keep track of the alternating
	var secondPhaseHasResultsPending bool

	remainingUnexhausted := len(queries) // when all queries are exhausted we can finally end this thing
	batchSizePerQuery := internal.BatchSizePerNumberOfQueries(limit, remainingUnexhausted)
	firstPhaseTotalPulled := 0

	exhaust := func(q int) {
		exhausted[q] = true
		remainingUnexhausted--
		if q == oldest.Q {
			oldest = internal.IterEvent{Q: -1}
		}
	}

	var firstPhaseResults []internal.IterEvent

	for q := range queries {
		cursor, err := txn.OpenCursor(queries[q].dbi)
		if err != nil {
			return nil, err
		}
		iterators[q] = &iterator{cursor: cursor}
		defer cursor.Close()
		iterators[q].seek(queries[q].startingPoint)
		results[q] = make([]internal.IterEvent, 0, batchSizePerQuery*2)
	}

	// fmt.Println("queries", len(queries))

	for c := 0; ; c++ {
		batchSizePerQuery = internal.BatchSizePerNumberOfQueries(limit, remainingUnexhausted)

		// fmt.Println("  iteration", c, "remaining", remainingUnexhausted, "batchsize", batchSizePerQuery)
		// we will go through all the iterators in batches until we have pulled all the required results
		for q, query := range queries {
			if exhausted[q] {
				continue
			}
			if oldest.Q == q && remainingUnexhausted > 1 {
				continue
			}
			// fmt.Println("    query", q, unsafe.Pointer(&results[q]), hex.EncodeToString(query.prefix), len(results[q]))

			it := iterators[q]
			pulledThisIteration := 0

			for {
				// we already have a k and a v and an err from the cursor setup, so check and use these
				if it.err != nil ||
					len(it.key) != query.keySize ||
					!bytes.HasPrefix(it.key, query.prefix) {
					// either iteration has errored or we reached the end of this prefix
					// fmt.Println("      reached end", it.key, query.keySize, query.prefix)
					exhaust(q)
					break
				}

				// "id" indexes don't contain a timestamp
				if query.timestampSize == 4 {
					createdAt := binary.BigEndian.Uint32(it.key[len(it.key)-4:])
					if createdAt < since {
						// fmt.Println("        reached since", createdAt, "<", since)
						exhaust(q)
						break
					}
				}

				// fetch actual event
				val, err := txn.Get(b.rawEventStore, it.valIdx)
				if err != nil {
					log.Printf(
						"lmdb: failed to get %x based on prefix %x, index key %x from raw event store: %s\n",
						it.valIdx, query.prefix, it.key, err)
					return nil, fmt.Errorf("iteration error: %w", err)
				}

				// check it against pubkeys without decoding the entire thing
				if extraAuthors != nil && !slices.Contains(extraAuthors, [32]byte(val[32:64])) {
					it.next()
					continue
				}

				// check it against kinds without decoding the entire thing
				if extraKinds != nil && !slices.Contains(extraKinds, [2]byte(val[132:134])) {
					it.next()
					continue
				}

				// decode the entire thing
				event := &nostr.Event{}
				if err := bin.Unmarshal(val, event); err != nil {
					log.Printf("lmdb: value read error (id %x) on query prefix %x sp %x dbi %d: %s\n", val[0:32],
						query.prefix, query.startingPoint, query.dbi, err)
					return nil, fmt.Errorf("event read error: %w", err)
				}

				// fmt.Println("      event", hex.EncodeToString(val[0:4]), "kind", binary.BigEndian.Uint16(val[132:134]), "author", hex.EncodeToString(val[32:36]), "ts", nostr.Timestamp(binary.BigEndian.Uint32(val[128:132])), hex.EncodeToString(it.key), it.valIdx)

				// if there is still a tag to be checked, do it now
				if extraTagValues != nil && !event.Tags.ContainsAny(extraTagKey, extraTagValues) {
					it.next()
					continue
				}

				// this event is good to be used
				evt := internal.IterEvent{Event: event, Q: q}
				//
				//
				if secondPhase {
					// do the process described below at HIWAWVRTP.
					// if we've reached here this means we've already passed the `since` check.
					// now we have to eliminate the event currently at the `since` threshold.
					nextThreshold := firstPhaseResults[len(firstPhaseResults)-2]
					if oldest.Event == nil {
						// fmt.Println("          b1", evt.ID[0:8])
						// BRANCH WHEN WE DON'T HAVE THE OLDEST EVENT (BWWDHTOE)
						// when we don't have the oldest set, we will keep the results
						//   and not change the cutting point -- it's bad, but hopefully not that bad.
						results[q] = append(results[q], evt)
						secondPhaseHasResultsPending = true
					} else if nextThreshold.CreatedAt > oldest.CreatedAt {
						// fmt.Println("          b2", nextThreshold.CreatedAt, ">", oldest.CreatedAt, evt.ID[0:8])
						// one of the events we have stored is the actual next threshold
						// eliminate last, update since with oldest
						firstPhaseResults = firstPhaseResults[0 : len(firstPhaseResults)-1]
						since = uint32(oldest.CreatedAt)
						// fmt.Println("            new since", since, evt.ID[0:8])
						//  we null the oldest Event as we can't rely on it anymore
						//   (we'll fall under BWWDHTOE above) until we have a new oldest set.
						oldest = internal.IterEvent{Q: -1}
						// anything we got that would be above this won't trigger an update to
						//   the oldest anyway, because it will be discarded as being after the limit.
						//
						// finally
						// add this to the results to be merged later
						results[q] = append(results[q], evt)
						secondPhaseHasResultsPending = true
					} else if nextThreshold.CreatedAt < evt.CreatedAt {
						// the next last event in the firstPhaseResults is the next threshold
						// fmt.Println("          b3", nextThreshold.CreatedAt, "<", oldest.CreatedAt, evt.ID[0:8])
						// eliminate last, update since with the antelast
						firstPhaseResults = firstPhaseResults[0 : len(firstPhaseResults)-1]
						since = uint32(nextThreshold.CreatedAt)
						// fmt.Println("            new since", since)
						// add this to the results to be merged later
						results[q] = append(results[q], evt)
						secondPhaseHasResultsPending = true
						// update the oldest event
						if evt.CreatedAt < oldest.CreatedAt {
							oldest = evt
						}
					} else {
						// fmt.Println("          b4", evt.ID[0:8])
						// oops, _we_ are the next `since` threshold
						firstPhaseResults[len(firstPhaseResults)-1] = evt
						since = uint32(evt.CreatedAt)
						// fmt.Println("            new since", since)
						// do not add us to the results to be merged later
						//   as we're already inhabiting the firstPhaseResults slice
					}
				} else {
					results[q] = append(results[q], evt)
					firstPhaseTotalPulled++

					// update the oldest event
					if oldest.Event == nil || evt.CreatedAt < oldest.CreatedAt {
						oldest = evt
					}
				}

				pulledPerQuery[q]++
				pulledThisIteration++
				if pulledThisIteration > batchSizePerQuery {
					// batch filled
					it.next()
					// fmt.Println("        filled", hex.EncodeToString(it.key), it.valIdx)
					break
				}
				if pulledPerQuery[q] >= limit {
					// batch filled + reached limit for this query (which is the global limit)
					exhaust(q)
					it.next()
					break
				}

				it.next()
			}
		}

		// we will do this check if we don't accumulated the requested number of events yet
		// fmt.Println("oldest", oldest.Event, "from iter", oldest.Q)
		if secondPhase && secondPhaseHasResultsPending && (oldest.Event == nil || remainingUnexhausted == 0) {
			// fmt.Println("second phase aggregation!")
			// when we are in the second phase we will aggressively aggregate results on every iteration
			//
			secondBatch = secondBatch[:0]
			for s := 0; s < len(secondPhaseParticipants); s++ {
				q := secondPhaseParticipants[s]

				if len(results[q]) > 0 {
					secondBatch = append(secondBatch, results[q])
				}

				if exhausted[q] {
					secondPhaseParticipants = internal.SwapDelete(secondPhaseParticipants, s)
					s--
				}
			}

			// every time we get here we will alternate between these A and B lists
			//   combining everything we have into a new partial results list.
			// after we've done that we can again set the oldest.
			// fmt.Println("  xxx", secondPhaseResultsToggle)
			if secondPhaseResultsToggle {
				secondBatch = append(secondBatch, secondPhaseResultsB)
				secondPhaseResultsA = internal.MergeSortMultiple(secondBatch, limit, secondPhaseResultsA)
				oldest = secondPhaseResultsA[len(secondPhaseResultsA)-1]
				// fmt.Println("  new aggregated a", len(secondPhaseResultsB))
			} else {
				secondBatch = append(secondBatch, secondPhaseResultsA)
				secondPhaseResultsB = internal.MergeSortMultiple(secondBatch, limit, secondPhaseResultsB)
				oldest = secondPhaseResultsB[len(secondPhaseResultsB)-1]
				// fmt.Println("  new aggregated b", len(secondPhaseResultsB))
			}
			secondPhaseResultsToggle = !secondPhaseResultsToggle

			since = uint32(oldest.CreatedAt)
			// fmt.Println("  new since", since)

			// reset the `results` list so we can keep using it
			results = results[:len(queries)]
			for _, q := range secondPhaseParticipants {
				results[q] = results[q][:0]
			}
		} else if !secondPhase && firstPhaseTotalPulled >= limit && remainingUnexhausted > 0 {
			// fmt.Println("have enough!", firstPhaseTotalPulled, "/", limit, "remaining", remainingUnexhausted)

			// we will exclude this oldest number as it is not relevant anymore
			// (we now want to keep track only of the oldest among the remaining iterators)
			oldest = internal.IterEvent{Q: -1}

			// HOW IT WORKS AFTER WE'VE REACHED THIS POINT (HIWAWVRTP)
			// now we can combine the results we have and check what is our current oldest event.
			// we also discard anything that is after the current cutting point (`limit`).
			// so if we have [1,2,3], [10, 15, 20] and [7, 21, 49] but we only want 6 total
			//   we can just keep [1,2,3,7,10,15] and discard [20, 21, 49],
			//   and also adjust our `since` parameter to `15`, discarding anything we get after it
			//   and immediately declaring that iterator exhausted.
			// also every time we get result that is more recent than this updated `since` we can
			//   keep it but also discard the previous since, moving the needle one back -- for example,
			//   if we get an `8` we can keep it and move the `since` parameter to `10`, discarding `15`
			//   in the process.
			all := make([][]internal.IterEvent, len(results))
			copy(all, results) // we have to use this otherwise internal.MergeSortMultiple will scramble our results slice
			firstPhaseResults = internal.MergeSortMultiple(all, limit, nil)
			oldest = firstPhaseResults[limit-1]
			since = uint32(oldest.CreatedAt)
			// fmt.Println("new since", since)

			for q := range queries {
				if exhausted[q] {
					continue
				}

				// we also automatically exhaust any of the iterators that have already passed the
				// cutting point (`since`)
				if results[q][len(results[q])-1].CreatedAt < oldest.CreatedAt {
					exhausted[q] = true
					remainingUnexhausted--
					continue
				}

				// for all the remaining iterators,
				// since we have merged all the events in this `firstPhaseResults` slice, we can empty the
				//   current `results` slices and reuse them.
				results[q] = results[q][:0]

				// build this index of indexes with everybody who remains
				secondPhaseParticipants = append(secondPhaseParticipants, q)
			}

			// we create these two lists and alternate between them so we don't have to create a
			//   a new one every time
			secondPhaseResultsA = make([]internal.IterEvent, 0, limit*2)
			secondPhaseResultsB = make([]internal.IterEvent, 0, limit*2)

			// from now on we won't run this block anymore
			secondPhase = true
		}

		// fmt.Println("remaining", remainingUnexhausted)
		if remainingUnexhausted == 0 {
			break
		}
	}

	// fmt.Println("is secondPhase?", secondPhase)

	var combinedResults []internal.IterEvent

	if secondPhase {
		// fmt.Println("ending second phase")
		// when we reach this point either secondPhaseResultsA or secondPhaseResultsB will be full of stuff,
		//   the other will be empty
		var secondPhaseResults []internal.IterEvent
		// fmt.Println("xxx", secondPhaseResultsToggle, len(secondPhaseResultsA), len(secondPhaseResultsB))
		if secondPhaseResultsToggle {
			secondPhaseResults = secondPhaseResultsB
			combinedResults = secondPhaseResultsA[0:limit] // reuse this
			// fmt.Println("  using b", len(secondPhaseResultsA))
		} else {
			secondPhaseResults = secondPhaseResultsA
			combinedResults = secondPhaseResultsB[0:limit] // reuse this
			// fmt.Println("  using a", len(secondPhaseResultsA))
		}

		all := [][]internal.IterEvent{firstPhaseResults, secondPhaseResults}
		combinedResults = internal.MergeSortMultiple(all, limit, combinedResults)
		// fmt.Println("final combinedResults", len(combinedResults), cap(combinedResults), limit)
	} else {
		combinedResults = make([]internal.IterEvent, limit)
		combinedResults = internal.MergeSortMultiple(results, limit, combinedResults)
	}

	return combinedResults, nil
}

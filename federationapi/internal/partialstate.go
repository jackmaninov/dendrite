// Copyright 2024 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/process"
)

const (
	partialStateWorkerCount = 4
	partialStateRetryDelay  = time.Minute * 5
)

// PartialStateWorker handles background resync of rooms with partial state from MSC3706 faster joins.
// After a partial state join, this worker fetches the full room state in the background.
type PartialStateWorker struct {
	process   *process.ProcessContext
	rsAPI     roomserverAPI.FederationRoomserverAPI
	fedAPI    *FederationInternalAPI
	workerCh  chan types.RoomNID
	retryMu   sync.Mutex
	retryMap  map[types.RoomNID]time.Time
}

// NewPartialStateWorker creates a new partial state worker
func NewPartialStateWorker(
	processCtx *process.ProcessContext,
	rsAPI roomserverAPI.FederationRoomserverAPI,
	fedAPI *FederationInternalAPI,
) *PartialStateWorker {
	return &PartialStateWorker{
		process:  processCtx,
		rsAPI:    rsAPI,
		fedAPI:   fedAPI,
		workerCh: make(chan types.RoomNID, 100),
		retryMap: make(map[types.RoomNID]time.Time),
	}
}

// Start begins the partial state worker, queuing all rooms with partial state for processing
func (w *PartialStateWorker) Start() error {
	// Start worker goroutines
	for i := 0; i < partialStateWorkerCount; i++ {
		go w.worker(i)
	}

	// Start retry goroutine
	go w.retryLoop()

	// Queue all rooms with partial state for processing
	roomNIDs, err := w.rsAPI.GetAllPartialStateRooms(w.process.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to load partial state rooms on startup")
		return err
	}

	if len(roomNIDs) > 0 {
		logrus.WithField("count", len(roomNIDs)).Info("Queuing partial state rooms for background resync")

		// Stagger the initial queue to avoid thundering herd
		offset := time.Second * 5
		step := time.Second
		if max := len(roomNIDs); max > 60 {
			step = (time.Second * 60) / time.Duration(max)
		}

		for _, roomNID := range roomNIDs {
			roomNID := roomNID
			time.AfterFunc(offset, func() {
				w.QueueRoom(roomNID)
			})
			offset += step
		}
	}

	return nil
}

// QueueRoom adds a room to the queue for partial state processing
func (w *PartialStateWorker) QueueRoom(roomNID types.RoomNID) {
	select {
	case w.workerCh <- roomNID:
	default:
		// Channel full, add to retry map
		w.retryMu.Lock()
		if _, exists := w.retryMap[roomNID]; !exists {
			w.retryMap[roomNID] = time.Now().Add(time.Second * 30)
		}
		w.retryMu.Unlock()
	}
}

// worker processes rooms from the channel
func (w *PartialStateWorker) worker(workerID int) {
	for roomNID := range w.workerCh {
		select {
		case <-w.process.Context().Done():
			return
		default:
		}

		if err := w.processRoom(roomNID); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"room_nid":  roomNID,
				"worker_id": workerID,
			}).Warn("Failed to resync partial state room, will retry")

			// Schedule retry
			w.retryMu.Lock()
			w.retryMap[roomNID] = time.Now().Add(partialStateRetryDelay)
			w.retryMu.Unlock()
		}
	}
}

// retryLoop periodically retries failed rooms
func (w *PartialStateWorker) retryLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.process.Context().Done():
			return
		case <-ticker.C:
			w.retryMu.Lock()
			now := time.Now()
			var toRetry []types.RoomNID
			for roomNID, retryAt := range w.retryMap {
				if now.After(retryAt) {
					toRetry = append(toRetry, roomNID)
				}
			}
			for _, roomNID := range toRetry {
				delete(w.retryMap, roomNID)
			}
			w.retryMu.Unlock()

			for _, roomNID := range toRetry {
				w.QueueRoom(roomNID)
			}
		}
	}
}

// processRoom fetches the full state for a room with partial state
func (w *PartialStateWorker) processRoom(roomNID types.RoomNID) error {
	ctx := w.process.Context()

	logger := logrus.WithField("room_nid", roomNID)

	// Check if room still has partial state
	hasPartialState, err := w.rsAPI.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return err
	}
	if !hasPartialState {
		logger.Debug("Room no longer has partial state, skipping")
		return nil
	}

	// Get servers in the room
	servers, err := w.rsAPI.GetPartialStateServers(ctx, roomNID)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		logger.Warn("No servers found for partial state room")
		return nil
	}

	// Get room ID from room NID
	roomID, err := w.rsAPI.RoomIDFromNID(ctx, roomNID)
	if err != nil {
		logger.WithError(err).Warn("Room not found for partial state room")
		// Clear partial state since room doesn't exist
		return w.rsAPI.ClearRoomPartialState(ctx, roomNID)
	}

	// Get room info for version
	roomInfo, err := w.rsAPI.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return err
	}
	if roomInfo == nil {
		logger.Warn("Room info not found for partial state room")
		return w.rsAPI.ClearRoomPartialState(ctx, roomNID)
	}

	logger = logger.WithField("room_id", roomID)
	logger.Info("Starting partial state resync")

	// Try each server until we succeed
	var lastErr error
	for _, serverStr := range servers {
		serverName := spec.ServerName(serverStr)

		// Get the latest events so we can fetch state at that point
		latestEventIDs, _, _, err := w.rsAPI.LatestEventIDs(ctx, roomNID)
		if err != nil {
			lastErr = err
			continue
		}
		if len(latestEventIDs) == 0 {
			logger.Warn("No latest events found")
			continue
		}

		// Fetch state from the remote server
		// We use the first latest event to get state at that point
		stateResponse, err := w.fedAPI.LookupState(
			ctx,
			w.fedAPI.cfg.Matrix.ServerName,
			serverName,
			roomID,
			latestEventIDs[0],
			roomInfo.RoomVersion,
		)
		if err != nil {
			logger.WithError(err).WithField("server", serverName).Warn("Failed to fetch state from server")
			lastErr = err
			continue
		}

		// Process the state - the events include member events we were missing
		stateEvents := stateResponse.GetStateEvents()
		authEvents := stateResponse.GetAuthEvents()

		logger.WithFields(logrus.Fields{
			"state_events": len(stateEvents.UntrustedEvents(roomInfo.RoomVersion)),
			"auth_events":  len(authEvents.UntrustedEvents(roomInfo.RoomVersion)),
			"server":       serverName,
		}).Info("Fetched full state for partial state room")

		// Send the state events to the roomserver
		if err := roomserverAPI.SendEventWithState(
			ctx,
			w.rsAPI,
			w.fedAPI.cfg.Matrix.ServerName,
			roomserverAPI.KindNew,
			stateResponse,
			nil, // No new event, just adding state
			serverName,
			nil,
			false,
		); err != nil {
			logger.WithError(err).Warn("Failed to send state to roomserver")
			lastErr = err
			continue
		}

		// Clear partial state flag - we've successfully synced
		if err := w.rsAPI.ClearRoomPartialState(ctx, roomNID); err != nil {
			logger.WithError(err).Error("Failed to clear partial state flag")
			return err
		}

		logger.Info("Successfully completed partial state resync")
		return nil
	}

	return lastErr
}

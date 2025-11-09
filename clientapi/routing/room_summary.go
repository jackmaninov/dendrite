// Copyright 2024 New Vector Ltd.
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/element-hq/dendrite/roomserver/api"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// RoomSummaryResponse represents the response for MSC3266 room summary API
type RoomSummaryResponse struct {
	RoomID          string   `json:"room_id"`
	RoomType        string   `json:"room_type,omitempty"`
	Name            string   `json:"name,omitempty"`
	Topic           string   `json:"topic,omitempty"`
	AvatarURL       string   `json:"avatar_url,omitempty"`
	CanonicalAlias  string   `json:"canonical_alias,omitempty"`
	NumJoinedMembers int     `json:"num_joined_members"`
	GuestCanJoin    bool     `json:"guest_can_join"`
	WorldReadable   bool     `json:"world_readable"`
	JoinRule        string   `json:"join_rule,omitempty"`
	AllowedRoomIDs  []string `json:"allowed_room_ids,omitempty"`
	Encryption      string   `json:"im.nheko.summary.encryption,omitempty"` // Unstable prefix
	Membership      string   `json:"membership,omitempty"`
	RoomVersion     string   `json:"im.nheko.summary.room_version,omitempty"` // Unstable prefix
}

// GetRoomSummary implements MSC3266 room summary API
// GET /_matrix/client/unstable/im.nheko.summary/summary/{roomIdOrAlias}
func GetRoomSummary(
	req *http.Request,
	device *userapi.Device,
	roomIDOrAlias string,
	rsAPI api.ClientRoomserverAPI,
) util.JSONResponse {
	ctx := req.Context()

	// Parse and validate room ID or alias
	roomID, err := parseRoomIDOrAlias(ctx, roomIDOrAlias, rsAPI)
	if err != nil {
		return *err
	}

	// Query room state
	stateRes := &api.QueryBulkStateContentResponse{}
	if err := rsAPI.QueryBulkStateContent(ctx, &api.QueryBulkStateContentRequest{
		RoomIDs:        []string{roomID},
		AllowWildcards: true,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			{EventType: spec.MRoomName, StateKey: ""},
			{EventType: spec.MRoomTopic, StateKey: ""},
			{EventType: spec.MRoomAvatar, StateKey: ""},
			{EventType: spec.MRoomCanonicalAlias, StateKey: ""},
			{EventType: spec.MRoomJoinRules, StateKey: ""},
			{EventType: spec.MRoomGuestAccess, StateKey: ""},
			{EventType: spec.MRoomHistoryVisibility, StateKey: ""},
			{EventType: spec.MRoomCreate, StateKey: ""},
			{EventType: spec.MRoomEncryption, StateKey: ""},
			{EventType: spec.MRoomMember, StateKey: "*"}, // Wildcard for member count
		},
	}, stateRes); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryBulkStateContent failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// Check if room exists
	roomState, ok := stateRes.Rooms[roomID]
	if !ok {
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("Room not found"),
		}
	}

	// Check access control (world-readable or user membership)
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("UserID is invalid")
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.Unknown("Device UserID is invalid"),
		}
	}

	canAccess, membership := checkRoomAccess(ctx, rsAPI, roomID, userID, roomState)
	if !canAccess {
		// Return 404 instead of 403 to not leak room existence
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("Room not found"),
		}
	}

	// Query room version
	roomVersion := getRoomVersion(ctx, rsAPI, roomID)

	// Build response
	response := buildRoomSummaryResponse(roomID, roomState, membership, roomVersion)

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: response,
	}
}

// parseRoomIDOrAlias resolves a room alias to room ID, or validates a room ID
func parseRoomIDOrAlias(ctx context.Context, roomIDOrAlias string, rsAPI api.ClientRoomserverAPI) (string, *util.JSONResponse) {
	// Try parsing as room ID first
	if roomID, err := spec.NewRoomID(roomIDOrAlias); err == nil {
		return roomID.String(), nil
	}

	// Try parsing as room alias - validate it has correct format
	_, _, err := gomatrixserverlib.SplitID('#', roomIDOrAlias)
	if err != nil {
		return "", &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("Invalid room ID or alias"),
		}
	}

	// Resolve alias to room ID
	queryReq := &api.GetRoomIDForAliasRequest{
		Alias:              roomIDOrAlias,
		IncludeAppservices: true,
	}
	queryRes := &api.GetRoomIDForAliasResponse{}
	if err := rsAPI.GetRoomIDForAlias(ctx, queryReq, queryRes); err != nil {
		util.GetLogger(ctx).WithError(err).Error("GetRoomIDForAlias failed")
		return "", &util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	if queryRes.RoomID == "" {
		return "", &util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("Room alias not found"),
		}
	}

	return queryRes.RoomID, nil
}

// checkRoomAccess determines if the user can access the room summary
// Returns (canAccess, membership)
func checkRoomAccess(
	ctx context.Context,
	rsAPI api.ClientRoomserverAPI,
	roomID string,
	userID spec.UserID,
	roomState map[gomatrixserverlib.StateKeyTuple]string,
) (bool, string) {
	// Get user's membership state (we'll need this regardless)
	membership := getUserMembership(ctx, rsAPI, roomID, userID)

	// Check if room is world-readable
	histVisKey := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomHistoryVisibility,
		StateKey:  "",
	}
	if visibility, ok := roomState[histVisKey]; ok && visibility == "world_readable" {
		// World-readable rooms can be accessed by anyone
		return true, membership
	}

	// Check if room is public (join_rule: "public")
	joinRuleKey := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomJoinRules,
		StateKey:  "",
	}
	if joinRuleContent, ok := roomState[joinRuleKey]; ok {
		var joinRules struct {
			JoinRule string `json:"join_rule"`
		}
		if err := json.Unmarshal([]byte(joinRuleContent), &joinRules); err == nil {
			if joinRules.JoinRule == "public" {
				// Public rooms can be previewed by anyone
				return true, membership
			}
		}
	}

	// Allow access if user is/was a member (join, invite, leave, ban)
	// This matches Synapse behavior - you can see summary of rooms you've been in
	if membership == "join" || membership == "invite" || membership == "leave" || membership == "ban" {
		return true, membership
	}

	// No access - not world-readable, not public, and user never joined
	return false, ""
}

// getUserMembership gets the current membership state for a user in a room
func getUserMembership(ctx context.Context, rsAPI api.ClientRoomserverAPI, roomID string, userID spec.UserID) string {
	var membershipRes api.QueryMembershipForUserResponse
	err := rsAPI.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{
		RoomID: roomID,
		UserID: userID,
	}, &membershipRes)

	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryMembershipForUser failed")
		return ""
	}

	return membershipRes.Membership
}

// getRoomVersion queries the room version
func getRoomVersion(ctx context.Context, rsAPI api.ClientRoomserverAPI, roomID string) string {
	roomVersion, err := rsAPI.QueryRoomVersionForRoom(ctx, roomID)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryRoomVersionForRoom failed")
		return ""
	}

	return string(roomVersion)
}

// buildRoomSummaryResponse constructs the response from room state
func buildRoomSummaryResponse(
	roomID string,
	roomState map[gomatrixserverlib.StateKeyTuple]string,
	membership string,
	roomVersion string,
) RoomSummaryResponse {
	response := RoomSummaryResponse{
		RoomID:        roomID,
		Membership:    membership,
		RoomVersion:   roomVersion,
		GuestCanJoin:  false,
		WorldReadable: false,
	}

	// Extract state content values
	for tuple, content := range roomState {
		switch tuple.EventType {
		case spec.MRoomName:
			response.Name = content

		case spec.MRoomTopic:
			response.Topic = content

		case spec.MRoomAvatar:
			response.AvatarURL = content

		case spec.MRoomCanonicalAlias:
			response.CanonicalAlias = content

		case spec.MRoomJoinRules:
			// Parse join rules content
			var joinRules struct {
				JoinRule string   `json:"join_rule"`
				Allow    []struct {
					RoomID string `json:"room_id"`
					Type   string `json:"type"`
				} `json:"allow"`
			}
			if err := json.Unmarshal([]byte(content), &joinRules); err == nil {
				response.JoinRule = joinRules.JoinRule

				// Extract allowed room IDs for restricted rooms
				if joinRules.JoinRule == "restricted" && len(joinRules.Allow) > 0 {
					allowedRooms := make([]string, 0, len(joinRules.Allow))
					for _, allow := range joinRules.Allow {
						if allow.Type == "m.room_membership" && allow.RoomID != "" {
							allowedRooms = append(allowedRooms, allow.RoomID)
						}
					}
					if len(allowedRooms) > 0 {
						response.AllowedRoomIDs = allowedRooms
					}
				}
			}

		case spec.MRoomGuestAccess:
			response.GuestCanJoin = content == "can_join"

		case spec.MRoomHistoryVisibility:
			response.WorldReadable = content == "world_readable"

		case spec.MRoomCreate:
			// Parse create event for room type
			var createContent struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(content), &createContent); err == nil {
				response.RoomType = createContent.Type
			}

		case spec.MRoomEncryption:
			// Parse encryption event for algorithm
			var encryptionContent struct {
				Algorithm string `json:"algorithm"`
			}
			if err := json.Unmarshal([]byte(content), &encryptionContent); err == nil {
				response.Encryption = encryptionContent.Algorithm
			}

		case spec.MRoomMember:
			// Count joined members
			if content == "join" {
				response.NumJoinedMembers++
			}
		}
	}

	return response
}

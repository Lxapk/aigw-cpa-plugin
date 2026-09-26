package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// This file implements the daily welfare that sits beside the growth tasks:
// the cat travel mini-game, the consecutive check-in streak, and the energy
// balance.
//
// Ported from the reference implementation's fetch_growth_summary and
// do_cat_travel (wb_tasks.py:135 and :365).

// growthTravelOutcome reports what a travel pass did.
type growthTravelOutcome struct {
	OK      bool
	Action  string // claim | depart | traveling | idle | unknown
	Credit  int
	Message string
}

// fetchGrowthSummary reads energy, streak and travel state.
//
// Each endpoint is queried independently: the reference implementation treats a
// dead endpoint as "unknown" rather than aborting, because reporting an energy
// balance is not worth failing the whole panel render over.
func (c *workBuddyClient) fetchGrowthSummary(ctx context.Context, creds *workBuddyCredentials) growthSummary {
	out := growthSummary{Travel: growthTravelInfo{State: "unknown"}}
	if creds == nil || creds.AccessToken == "" {
		return out
	}

	if env, ok := c.growthGet(ctx, creds, workBuddyEnergyPath); ok {
		var data struct {
			Balance int `json:"balance"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		out.Energy = data.Balance
	}

	if env, ok := c.growthGet(ctx, creds, workBuddyStreakPath); ok {
		var data struct {
			Streak struct {
				Days int `json:"days"`
			} `json:"streak"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		out.StreakDays = data.Streak.Days
	}

	if env, ok := c.growthGet(ctx, creds, workBuddyTravelStatusPath); ok {
		var data struct {
			State             string `json:"state"`
			DailyLimitReached bool   `json:"daily_limit_reached"`
			Location          struct {
				Name string `json:"name"`
			} `json:"location"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		out.Travel = growthTravelInfo{
			State:             firstNonEmpty(data.State, "unknown"),
			DailyLimitReached: data.DailyLimitReached,
			Location:          data.Location.Name,
		}
	}
	return out
}

// growthGet performs a GET and returns the envelope when code == 0.
func (c *workBuddyClient) growthGet(ctx context.Context, creds *workBuddyCredentials, path string) (growthEnvelope, bool) {
	status, raw, errRequest := c.growthRequest(ctx, http.MethodGet, growthBase(creds), path, creds, nil, nil)
	if errRequest != nil {
		return growthEnvelope{}, false
	}
	env, ok := decodeGrowthEnvelope(raw)
	if !ok {
		_ = status
		return env, false
	}
	return env, true
}

// travel performs one cat-travel pass: claim an arrived reward, or send the cat
// out when idle.
//
// The state machine is:
//
//	arrived   -> claim
//	idle      -> depart (unless the daily limit is reached)
//	traveling -> nothing to do
func (r *growthRunner) travel(ctx context.Context, creds *workBuddyCredentials) (growthTravelOutcome, error) {
	if errWait := r.spacing(ctx, creds); errWait != nil {
		return growthTravelOutcome{}, errWait
	}
	summary := r.client.fetchGrowthSummary(ctx, creds)
	state := summary.Travel.State

	switch state {
	case "arrived":
		if errWait := r.spacing(ctx, creds); errWait != nil {
			return growthTravelOutcome{}, errWait
		}
		credit, errClaim := r.client.claimTravel(ctx, creds)
		if errClaim != nil {
			return growthTravelOutcome{Message: errClaim.Error()}, nil
		}
		return growthTravelOutcome{
			OK: true, Action: "claim", Credit: credit,
			Message: fmt.Sprintf("旅行归来领奖成功！获得 %d 积分", credit),
		}, nil

	case "idle":
		if summary.Travel.DailyLimitReached {
			return growthTravelOutcome{
				OK: true, Action: "idle",
				Message: "猫猫今日已完成旅行，明日 00:00 刷新",
			}, nil
		}
		if errWait := r.spacing(ctx, creds); errWait != nil {
			return growthTravelOutcome{}, errWait
		}
		location, errDepart := r.client.departTravel(ctx, creds)
		if errDepart != nil {
			return growthTravelOutcome{Message: errDepart.Error()}, nil
		}
		return growthTravelOutcome{
			OK: true, Action: "depart",
			Message: fmt.Sprintf("猫猫已出发前往「%s」，预计数小时后归来！", location),
		}, nil

	case "traveling":
		return growthTravelOutcome{
			OK: true, Action: "traveling",
			Message: "猫猫正在旅行途中，请稍后再来查看！",
		}, nil
	}

	return growthTravelOutcome{
		OK: true, Action: firstNonEmpty(state, "unknown"),
		Message: fmt.Sprintf("当前状态: %s", firstNonEmpty(state, "unknown")),
	}, nil
}

// claimTravel collects a finished trip's reward.
func (c *workBuddyClient) claimTravel(ctx context.Context, creds *workBuddyCredentials) (int, error) {
	status, raw, errRequest := c.growthRequest(ctx, http.MethodPost, growthBase(creds), workBuddyTravelClaimPath, creds, map[string]any{}, nil)
	if errRequest != nil {
		return 0, errRequest
	}
	env, ok := decodeGrowthEnvelope(raw)
	if !ok {
		return 0, fmt.Errorf("领奖失败: %s", growthMessage(env, status))
	}
	var data struct {
		RewardCredit int `json:"reward_credit"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	return data.RewardCredit, nil
}

// departTravel sends the cat travelling, returning the destination name.
//
// The depart call requires a body carrying location_id; an empty body is
// rejected with 400 "invalid request". The destination list comes from
// travel/config, and the first entry is used as the reference implementation
// does, falling back to id 1 when the list cannot be read.
func (c *workBuddyClient) departTravel(ctx context.Context, creds *workBuddyCredentials) (string, error) {
	locationID := 1
	if env, ok := c.growthGet(ctx, creds, workBuddyTravelConfigPath); ok {
		var data struct {
			Locations []struct {
				ID int `json:"id"`
			} `json:"locations"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		if len(data.Locations) > 0 {
			locationID = data.Locations[0].ID
		}
	}

	status, raw, errRequest := c.growthRequest(ctx, http.MethodPost, growthBase(creds), workBuddyTravelDepartPath, creds,
		map[string]any{"location_id": locationID}, nil)
	if errRequest != nil {
		return "", errRequest
	}
	env, ok := decodeGrowthEnvelope(raw)
	if !ok {
		return "", fmt.Errorf("派出旅行被上游拒绝: %s", growthMessage(env, status))
	}
	var data struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	return data.Location.Name, nil
}

// Msg adapts an error into the outcome's message field.
//
// The travel helpers return an error rather than a shaped outcome because the
// caller wants a message either way; this keeps the switch above readable.
func Msg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

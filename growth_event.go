package main

import (
	"fmt"
	"strconv"
	"time"
)

// This file builds the synthetic event payloads that light up growth tasks.
//
// Ported from the reference implementation's build_event (wb_tasks.py:278),
// which reverse engineered the desktop client's /v2/report traffic.
//
// Two rules matter more than the individual field values:
//
//  1. Every event carries a fresh timestamp, conversationId and requestId. The
//     upstream treats a repeated identity as a duplicate and does not advance
//     progress.
//  2. Expert and team events additionally key on "id". The upstream
//     de-duplicates by (eventCode, id) and only counts ids it has not seen, so
//     a task targeting 5 reports needs 5 distinct ids — repeating one id
//     advances the task exactly once no matter how many times it is sent.

// growthEvent is one report entry.
//
// It is a map rather than a struct because each task kind contributes a
// different field set and the upstream is tolerant of extra/missing keys; a
// struct with dozens of omitempty fields would obscure that.
type growthEvent = map[string]any

// growthEventClock is injectable so tests get deterministic timestamps.
var growthEventClock = func() time.Time { return time.Now() }

// buildGrowthEvent constructs the event for one task kind.
//
// idx distinguishes repeated reports for the same task; expert is the
// (id, name) pair to use for expert/team/lighthouse events.
func buildGrowthEvent(creds *workBuddyCredentials, kind string, idx int, expert *struct{ ID, Name string }) growthEvent {
	now := growthEventClock().UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", now, idx)
	rid := cid + "-req"
	uid := ""
	if creds != nil {
		uid = creds.UID
	}

	switch kind {
	case "canvas":
		return growthEvent{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": now,
			"reportDelay": 0, "conversationId": cid, "requestId": rid,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", now), "cost": 0,
			"isSuccessful": true, "userId": uid,
		}

	case "template":
		return growthEvent{
			"eventCode": "agent_task_created_with_template", "timestamp": now,
			"reportDelay": 0, "isCustomModel": true, "id": strconv.Itoa(idx),
			"name": "幻灯片", "requestId": rid, "conversationId": cid, "userId": uid,
		}

	case "expert", "team", "lighthouse":
		expertType := "agent"
		if kind == "team" {
			expertType = "team"
		}
		var exID, exName string
		switch {
		case expert != nil:
			exID, exName = expert.ID, expert.Name
		case kind == "lighthouse":
			exID, exName = "ex_2cvvUZQhDyeJ", "腾讯轻量云专家"
		case kind == "team":
			exID, exName = "CloudOpsTeam", "运维专家团队"
		default:
			exID, exName = "ContentCreator", "内容创作专家"
		}
		return growthEvent{
			"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": exID, "name": exName, "expertTitle": exName,
			"type": "02-Engineering", "expertType": expertType, "source": "builtin",
			"version": "1.0.2", "cost": 0, "characterCount": 12, "conversationId": cid,
			"requestId": rid, "messageId": rid, "requestModelId": "deepseek-v4-flash",
			"requestModelName": "DeepSeek V4 Flash", "userId": uid,
		}

	case "skill":
		return growthEvent{
			"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
			"skillId": "skill_2096525080079265792", "name": "pptx", "userId": uid,
		}

	case "automation":
		return growthEvent{
			"eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
			"name": "每周工作整理", "type": "cron", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"conversationId": cid, "requestId": rid,
			"schedule": map[string]any{
				"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0",
			},
			"prompt": "每周五自动整理本周工作", "userId": uid,
		}

	case "playbook":
		return growthEvent{
			"eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
			"id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
			"type": "other", "promptLength": 10, "isOfficial": 1,
			"source": "discover", "conversationId": cid, "requestId": rid, "userId": uid,
		}

	case "skin":
		return growthEvent{
			"eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
			"action": "apply", "source": "settings_close", "id": "theme-tkmw7j",
			"vipLevel": "free", "series": "craft", "type": "unknown",
			"name": "和平精英激战金秋", "userId": uid,
		}

	case "chat", "glmchat", "cat":
		modelID, modelName := "deepseek-v4-flash", "DeepSeek V4 Flash"
		if kind == "glmchat" || kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		mode := "craft"
		if kind == "cat" {
			// The night-owl task only counts reports tagged with the night mode.
			mode = "night"
		}
		return growthEvent{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": rid,
			"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "agentName": "default", "agentType": "conversation",
			"userId": uid,
		}
	}

	// Unknown kinds degrade to a heartbeat rather than sending a malformed
	// event with a fabricated eventCode.
	//
	// buddy_first is deliberately absent: the reference implementation's
	// build_event has no branch for it either, because the upstream grants the
	// buddy only through a real client action. Reporting a synthetic event would
	// be ignored at best.
	return growthEvent{"eventCode": "heartbeat", "timestamp": now, "userId": uid}
}

// growthExpertFor selects the id to use for the n-th report of a task.
//
// Cycling through the pool is what lets a multi-target task progress: the
// upstream only counts ids it has not seen before.
func growthExpertFor(kind string, index int) *struct{ ID, Name string } {
	var pool []struct{ ID, Name string }
	switch kind {
	case "team":
		pool = growthTeamIDs
	case "expert":
		pool = growthExpertIDs
	default:
		return nil
	}
	if len(pool) == 0 {
		return nil
	}
	if index < 0 {
		index = -index
	}
	entry := pool[index%len(pool)]
	return &struct{ ID, Name string }{ID: entry.ID, Name: entry.Name}
}

// inNightWindow reports whether the current local hour falls inside the
// 23:00-08:00 window the night-owl task requires.
func inNightWindow(now time.Time) bool {
	hour := now.Hour()
	return hour >= 23 || hour < 8
}

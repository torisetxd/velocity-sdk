package context

import (
	stdctx "context"
	"fmt"
	"io"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

type HandoffResult struct {
	History          []Event
	Summary          string
	ModifiedFiles    []string
	DiffText         string
	ParentSessionIDs []string
	TotalTokens      int
}

func GenerateHandoff(ctx stdctx.Context, p provider.Provider, model ModelRef, modelExtra map[string]any, history []Event) (HandoffResult, error) {
	if p == nil {
		return HandoffResult{}, fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(model.Model) == "" {
		return HandoffResult{}, fmt.Errorf("model is required")
	}

	events := ActiveHistory(history)
	if len(nonSystemEvents(events)) == 0 {
		return HandoffResult{}, fmt.Errorf("no conversation to handoff")
	}

	conversationText := FormatEventsForCompression(events)
	systemPrompt := handoffSystemPrompt()
	userContent := "Generate a handoff artifact for this conversation.\n\n" + conversationText

	req := provider.ChatRequest{
		Model: model.Model,
		Messages: []provider.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Stream:    true,
		ExtraBody: provider.CopyMapAny(modelExtra),
	}

	stream, err := p.StreamChat(ctx, req)
	if err != nil {
		return HandoffResult{}, err
	}
	defer stream.Close()

	var summary strings.Builder
	totalTokens := 0
	for {
		if err := ctx.Err(); err != nil {
			return HandoffResult{}, err
		}
		ev, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			return HandoffResult{}, recvErr
		}
		if ev.Content != "" {
			summary.WriteString(ev.Content)
		}
		if ev.TotalTokens > 0 {
			totalTokens = ev.TotalTokens
		}
	}

	compressedSummary := strings.TrimSpace(summary.String())
	if compressedSummary == "" {
		return HandoffResult{}, fmt.Errorf("failed to generate handoff summary")
	}

	return HandoffResult{
		History:     events,
		Summary:     compressedSummary,
		TotalTokens: totalTokens,
	}, nil
}

func handoffSystemPrompt() string {
	return `You are a Senior Technical Lead performing a "Handoff" procedure to a fresh agent instance. The current context window is closing due to token limits.

Your goal is to generate a structured "Handoff Artifact" that provides the next agent with immediate situational awareness WITHOUT carrying over bloat.

## Rules for Compression
1. **NO CODE SNIPPETS:** Do not include code unless it is a single critical line (e.g., a regex or a command). The agent can read files; it just needs to know *which* files.
2. **NO TRIVIAL LOGS:** Do not summarize "User said hi, Assistant said hi."
3. **STATE OVER HISTORY:** Focus on *where we are now*, not *how we got here*.

## The Handoff Artifact Structure (Output this exact format)

# Session Handoff

## Current Objectives
- **Primary Goal:** [One sentence summary]
- **Immediate Next Step:** [What exactly should the agent do next?]

## Active Context (The "Map")
*Files currently being modified or analyzed:*
- path/to/file.ext - [Brief note: e.g., "Contains the broken auth logic"]
- path/to/another.ext - [Brief note: e.g., "Reference implementation"]

## Task Status
- [x] [Completed task]
- [/] [In-progress task] - **Current Focus**
- [ ] [Pending task]

## Knowledge & Decisions (Crucial)
*High-level architectural decisions or constraints identified:*
- [Decision/Constraint 1]
- [Decision/Constraint 2]

## Dead Ends & Anti-Patterns (Edge Case Prevention)
*Approaches we already tried that FAILED. Prevent loops:*
- Tried [Strategy X], failed because [Reason]. **DO NOT RETRY.**
- Avoid modifying [Function Y] as it breaks [Dependency Z].`
}

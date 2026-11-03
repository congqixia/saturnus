package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

const larkBegin = "[Saturnus task details]"
const larkEnd = "[/Saturnus task details]"

func (s *Service) AutoLarkEnabled() bool { return s.cfg.CreateLarkTask }

func (s *Service) ResetLarkCreation(ctx context.Context, id string) error {
	if !s.cfg.Enabled {
		return ErrDisabled
	}
	unlock, err := s.locks.Lock(ctx, "lark:"+id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.store.ResetTaskLarkCreation(id)
}

// LinkLark recovers from an uncertain create outcome without creating another task.
func (s *Service) LinkLark(ctx context.Context, id, guid string) (store.TaskLarkLink, error) {
	if !s.cfg.Enabled {
		return store.TaskLarkLink{}, ErrDisabled
	}
	if s.cfg.Lark == nil || !s.cfg.Lark.Enabled() {
		return store.TaskLarkLink{}, errors.New("Lark task integration is unavailable")
	}
	guid = strings.TrimSpace(guid)
	if guid == "" {
		return store.TaskLarkLink{}, errors.New("Lark GUID is required")
	}
	unlock, err := s.locks.Lock(ctx, "lark:"+id)
	if err != nil {
		return store.TaskLarkLink{}, err
	}
	defer unlock()
	if _, err := s.store.GetTask(id); err != nil {
		return store.TaskLarkLink{}, err
	}
	remote, err := s.cfg.Lark.GetTaskContext(ctx, guid)
	if err != nil {
		return store.TaskLarkLink{}, err
	}
	if remote.GUID != guid {
		return store.TaskLarkLink{}, errors.New("Lark returned a different task GUID")
	}
	if err := s.store.ReserveTaskLarkCreation(id, "{}"); err != nil {
		return store.TaskLarkLink{}, err
	}
	if err := s.store.SetTaskLarkRemote(id, guid, remote.URL); err != nil {
		return store.TaskLarkLink{}, err
	}
	return s.store.GetTaskLarkLink(id)
}

// SyncLark creates a missing task or updates an existing one. A persisted,
// immutable create request makes immediate retries safe within Lark's token window.
func (s *Service) SyncLark(ctx context.Context, id, assignee, groupID string) (link store.TaskLarkLink, retErr error) {
	if !s.cfg.Enabled {
		return link, ErrDisabled
	}
	if s.cfg.Lark == nil || !s.cfg.Lark.Enabled() {
		return link, errors.New("Lark task integration is unavailable")
	}
	unlock, err := s.locks.Lock(ctx, "lark:"+id)
	if err != nil {
		return link, err
	}
	defer unlock()
	task, err := s.store.GetTask(id)
	if err != nil {
		return link, err
	}
	link, err = s.store.GetTaskLarkLink(id)
	if errors.Is(err, store.ErrNotFound) {
		if assignee == "" {
			assignee = s.cfg.LarkAssigneeOpenID
		}
		if assignee == "" {
			return link, errors.New("an assignee open_id is required; configure SATURNUS_TASKS_LARK_ASSIGNEE_OPEN_ID or use the bot")
		}
		description, descErr := s.larkDescription(task, "")
		if descErr != nil {
			return link, descErr
		}
		input := lark.CreateTaskInput{Summary: task.Title, Description: description, MemberOpenIDs: []string{assignee}, ClientToken: task.ID}
		data, marshalErr := json.Marshal(input)
		if marshalErr != nil {
			return link, marshalErr
		}
		if err := s.store.ReserveTaskLarkCreation(id, string(data)); err != nil {
			return link, err
		}
		link, err = s.store.GetTaskLarkLink(id)
	}
	if err != nil {
		return link, err
	}
	if groupID == "" {
		groupID = link.GroupID
		if groupID == "" {
			groupID = s.cfg.LarkTaskGroupID
		}
	}
	defer func() {
		msg := ""
		if retErr != nil {
			msg = retErr.Error()
		}
		if err := s.store.SetTaskLarkSync(id, msg, retErr == nil); err != nil {
			retErr = errors.Join(retErr, err)
		}
		if saved, err := s.store.GetTaskLarkLink(id); err == nil {
			link = saved
		}
	}()
	if link.GUID == "" {
		started, err := time.Parse(time.RFC3339Nano, link.StartedAt)
		if err != nil || time.Since(started) >= 4*time.Minute {
			return link, errors.New("Lark task creation outcome is uncertain; if it exists use /sat task link-lark <task_id> <lark_guid>, otherwise confirm it does not exist and use /sat task reset-lark-create <task_id>")
		}
		var input lark.CreateTaskInput
		if err := json.Unmarshal([]byte(link.RequestJSON), &input); err != nil {
			return link, err
		}
		remote, err := s.cfg.Lark.CreateTask(input)
		if err != nil {
			return link, fmt.Errorf("create Lark task: %w", err)
		}
		if remote.GUID == "" {
			return link, errors.New("Lark returned no task GUID")
		}
		if err := s.store.SetTaskLarkRemote(id, remote.GUID, remote.URL); err != nil {
			return link, fmt.Errorf("persist Lark task %s: %w", remote.GUID, err)
		}
		link.GUID, link.URL = remote.GUID, remote.URL
	}
	if groupID != "" && groupID != link.GroupID {
		if err := s.cfg.Lark.AddTaskToList(link.GUID, groupID); err != nil {
			return link, fmt.Errorf("add Lark task to group: %w", err)
		}
		if err := s.store.SetTaskLarkGroup(id, groupID); err != nil {
			return link, err
		}
	}
	remote, err := s.cfg.Lark.GetTaskContext(ctx, link.GUID)
	if err != nil {
		return link, fmt.Errorf("get Lark task: %w", err)
	}
	description, err := s.larkDescription(task, remote.Description)
	if err != nil {
		return link, err
	}
	update := lark.UpdateTaskInput{}
	if remote.Summary != task.Title {
		update.Summary = &task.Title
	}
	if remote.Description != description {
		update.Description = &description
	}
	if (task.Status == store.TaskStatusDone || task.Status == store.TaskStatusArchived) && !remote.Completed {
		completed := true
		update.Completed = &completed
	}
	if update.Summary != nil || update.Description != nil || update.Completed != nil {
		if err := s.cfg.Lark.UpdateTaskContext(ctx, link.GUID, update); err != nil {
			return link, fmt.Errorf("update Lark task: %w", err)
		}
	}
	return link, nil
}

func (s *Service) larkDescription(task store.Task, current string) (string, error) {
	start, end := strings.Index(current, larkBegin), strings.Index(current, larkEnd)
	if (start >= 0) != (end >= 0) || (start >= 0 && end < start) {
		return "", errors.New("invalid Saturnus block in Lark task description")
	}
	before, after := strings.TrimSpace(current), ""
	if start >= 0 {
		before = strings.TrimSpace(current[:start])
		after = strings.TrimSpace(current[end+len(larkEnd):])
	}
	refs, err := s.store.ListTaskRefs(task.ID)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Task: %s\nStatus: %s\n", task.ID, task.Status)
	if task.Description != "" {
		fmt.Fprintf(&body, "Description: %s\n", task.Description)
	}
	if task.IssueURL != "" {
		fmt.Fprintf(&body, "Issue: %s\n", task.IssueURL)
	}
	for _, pr := range s.store.ListTaskPRs(task.ID) {
		fmt.Fprintf(&body, "PR: %s (%s)\n", pr.PRURL, pr.PRState)
	}
	for _, ref := range refs {
		fmt.Fprintf(&body, "Ref: %s %s\n", ref.Title, ref.URL)
	}
	prefix := ""
	if before != "" {
		prefix = before + "\n\n"
	}
	suffix := ""
	if after != "" {
		suffix = "\n\n" + after
	}
	available := lark.TaskDescriptionMaxRunes - utf8.RuneCountInString(prefix+suffix+larkBegin+"\n\n"+larkEnd)
	if available < 0 {
		return "", errors.New("existing Lark description leaves no room for Saturnus details")
	}
	// User-provided task text must not be mistaken for the managed delimiters.
	managed := strings.ReplaceAll(body.String(), larkBegin, "[Saturnus task details (text)]")
	managed = strings.ReplaceAll(managed, larkEnd, "[/Saturnus task details (text)]")
	runes := []rune(strings.TrimSpace(managed))
	if len(runes) > available {
		runes = runes[:available]
	}
	return prefix + larkBegin + "\n" + string(runes) + "\n" + larkEnd + suffix, nil
}

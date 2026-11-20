package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Kind string `json:"kind"` // image | video | audio | file
}

type Activity struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // tool | thinking | status
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Status     string `json:"status"` // running | done | error
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

type Message struct {
	ID         int64        `json:"id"`
	Role       string       `json:"role"` // user | assistant | error
	Text       string       `json:"text"`
	Files      []Attachment `json:"files,omitempty"`
	Activities []Activity   `json:"activities,omitempty"`
	Time       int64        `json:"time"` // unix seconds
}

type Thread struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	SessionName string    `json:"sessionName"` // passed to hermes --continue
	SessionID   string    `json:"sessionId,omitempty"`
	Model       string    `json:"model,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	Archived    bool      `json:"archived,omitempty"`
	Created     int64     `json:"created"`
	Messages    []Message `json:"messages"`
}

type ThreadLite struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Model    string `json:"model,omitempty"`
	Pinned   bool   `json:"pinned,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	Created  int64  `json:"created"`
	LastTime int64  `json:"lastTime"`
}

type SearchHit struct {
	ThreadID    string `json:"threadId"`
	ThreadTitle string `json:"threadTitle"`
	Archived    bool   `json:"archived,omitempty"`
	MsgID       int64  `json:"msgId,omitempty"` // 0 when matching thread title only
	Role        string `json:"role,omitempty"`
	Snippet     string `json:"snippet"`
	Time        int64  `json:"time"`
}

type UISnap struct {
	Threads  []ThreadLite `json:"threads"`
	Active   string       `json:"active"`
	Messages []Message    `json:"messages"`
}

type Store struct {
	mu      sync.Mutex
	path    string
	threads []*Thread
	active  string
	nextMsg int64
	nextThr int64
}

func NewStore(dataDir string) *Store {
	s := &Store{path: filepath.Join(dataDir, "state.json"), nextMsg: 1, nextThr: 1}
	if b, err := os.ReadFile(s.path); err == nil {
		var raw struct {
			Threads []*Thread `json:"threads"`
			Active  string    `json:"active"`
			NextMsg int64     `json:"nextMsg"`
			NextThr int64     `json:"nextThr"`
		}
		if json.Unmarshal(b, &raw) == nil && raw.Threads != nil {
			s.threads = raw.Threads
			s.active = raw.Active
			s.nextMsg = raw.NextMsg
			s.nextThr = raw.NextThr
		}
	}
	if s.nextMsg < 1 {
		s.nextMsg = 1
	}
	if s.nextThr < 1 {
		s.nextThr = 1
	}
	if s.active == "" && len(s.threads) > 0 {
		s.active = s.threads[0].ID
	}
	return s
}

func nowSec() int64 { return time.Now().Unix() }

func (s *Store) saveLocked() {
	raw := struct {
		Threads []*Thread `json:"threads"`
		Active  string    `json:"active"`
		NextMsg int64     `json:"nextMsg"`
		NextThr int64     `json:"nextThr"`
	}{s.threads, s.active, s.nextMsg, s.nextThr}
	b, err := json.MarshalIndent(raw, "", " ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err == nil {
		if err := os.Rename(tmp, s.path); err != nil {
			log.Printf("webchat: state rename failed: %v", err)
		}
	}
}

func lite(t *Thread) ThreadLite {
	lt := t.Created
	if len(t.Messages) > 0 {
		lt = t.Messages[len(t.Messages)-1].Time
	}
	return ThreadLite{
		ID:       t.ID,
		Title:    t.Title,
		Model:    t.Model,
		Pinned:   t.Pinned,
		Archived: t.Archived,
		Created:  t.Created,
		LastTime: lt,
	}
}

func (s *Store) find(id string) *Thread {
	for _, t := range s.threads {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *Store) newThreadLocked() ThreadLite {
	id := fmt.Sprintf("t%d", s.nextThr)
	s.nextThr++
	t := &Thread{
		ID:          id,
		Title:       "New chat",
		Created:     nowSec(),
		SessionName: "webchat-" + time.Now().Format("20060102-150405") + "-" + id,
	}
	s.threads = append([]*Thread{t}, s.threads...)
	s.active = id
	s.saveLocked()
	return lite(t)
}

func (s *Store) NewThread() ThreadLite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newThreadLocked()
}

func (s *Store) SetActive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(id) == nil {
		return false
	}
	s.active = id
	s.saveLocked()
	return true
}

func (s *Store) Snapshot() UISnap {
	return s.SnapshotFor("")
}

func (s *Store) SnapshotFor(threadID string) UISnap {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.active
	if threadID != "" && s.find(threadID) != nil {
		active = threadID
	}
	snap := UISnap{Active: active}
	for _, t := range s.threads {
		snap.Threads = append(snap.Threads, lite(t))
	}
	if t := s.find(active); t != nil {
		msgs := t.Messages
		if len(msgs) > 400 {
			msgs = msgs[len(msgs)-400:]
		}
		snap.Messages = append(snap.Messages, msgs...)
	}
	return snap
}

func (s *Store) ThreadByID(id string) (Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return Thread{}, false
	}
	cp := *t
	cp.Messages = append([]Message(nil), t.Messages...)
	return cp, true
}

func (s *Store) HasAttachment(cleanPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.threads {
		for _, m := range t.Messages {
			for _, f := range m.Files {
				if filepath.Clean(f.Path) == cleanPath {
					return true
				}
			}
		}
	}
	return false
}

func (s *Store) ReferencedPaths() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, t := range s.threads {
		for _, m := range t.Messages {
			for _, f := range m.Files {
				if f.Path != "" {
					out[filepath.Clean(f.Path)] = true
				}
			}
		}
	}
	return out
}

func (s *Store) SessionName(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(threadID); t != nil {
		return t.SessionName
	}
	return ""
}

func validModelName(m string) bool {
	if m == "" {
		return true
	}
	if len(m) > 120 || strings.HasPrefix(m, "-") {
		return false
	}
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

func (s *Store) Model(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(threadID); t != nil {
		return t.Model
	}
	return ""
}

func (s *Store) SetModel(id, model string) bool {
	model = strings.TrimSpace(model)
	if strings.EqualFold(model, "default") || strings.EqualFold(model, "reset") {
		model = ""
	}
	if !validModelName(model) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return false
	}
	t.Model = model
	s.saveLocked()
	return true
}

func (s *Store) UsedModels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, t := range s.threads {
		if t.Model != "" && !seen[t.Model] {
			seen[t.Model] = true
			out = append(out, t.Model)
		}
	}
	return out
}

func shortTitle(text string, files []Attachment) string {
	line := strings.TrimSpace(text)
	if line == "" && len(files) > 0 {
		return "📎 " + files[0].Name
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 48 {
		line = line[:48] + "…"
	}
	if line == "" {
		line = "New chat"
	}
	return line
}

func (s *Store) AddUser(text string, files []Attachment) (ThreadLite, Message) {
	return s.AddUserTo("", text, files)
}

func (s *Store) AddUserTo(threadID, text string, files []Attachment) (ThreadLite, Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threadID == "" {
		threadID = s.active
	}
	t := s.find(threadID)
	if t == nil {
		l := s.newThreadLocked()
		t = s.find(l.ID)
	}
	id := s.nextMsg
	s.nextMsg++
	m := Message{ID: id, Role: "user", Text: text, Files: files, Time: nowSec()}
	t.Messages = append(t.Messages, m)
	if t.Title == "New chat" {
		t.Title = shortTitle(text, files)
	}
	if len(t.Messages) > 500 {
		t.Messages = t.Messages[len(t.Messages)-500:]
	}
	s.saveLocked()
	return lite(t), m
}

func (s *Store) ReserveMsgID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextMsg
	s.nextMsg++
	return id
}

func (s *Store) FinalizeAssistant(threadID string, id int64, text string, files []Attachment, activities []Activity, sessionID string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Message{ID: id, Role: "assistant", Text: text, Files: files, Activities: activities, Time: nowSec()}
	if t := s.find(threadID); t != nil {
		t.Messages = append(t.Messages, m)
		if sessionID != "" {
			t.SessionID = sessionID
		}
		if len(t.Messages) > 500 {
			t.Messages = t.Messages[len(t.Messages)-500:]
		}
		s.saveLocked()
	}
	return m
}

// AddNotice appends a Sentinel-generated message to a thread without involving
// the agent: role "assistant" for a plain notice, "error" for a failure.
func (s *Store) AddNotice(threadID, role, text string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextMsg
	s.nextMsg++
	m := Message{ID: id, Role: role, Text: text, Time: nowSec()}
	t := s.find(threadID)
	if t == nil {
		t = s.find(s.active)
	}
	if t == nil {
		// Nothing to append to yet (fresh data dir): open the thread the first
		// message would have created so the notice is never lost.
		t = s.find(s.newThreadLocked().ID)
	}
	if t != nil {
		t.Messages = append(t.Messages, m)
		if len(t.Messages) > 500 {
			t.Messages = t.Messages[len(t.Messages)-500:]
		}
		s.saveLocked()
	}
	return m
}

func (s *Store) AddError(threadID string, text string) Message {
	return s.AddNotice(threadID, "error", text)
}

// AddDelivered appends a message pushed in by the gateway (cron output and
// notification deliveries). Unlike AddNotice it never falls back to the active
// thread: a delivery addressed to a thread that no longer exists must fail
// loudly so the sender sees it, not land somewhere unrelated. An empty threadID
// means the active thread, and title names a still-unnamed thread.
func (s *Store) AddDelivered(threadID, role, text, title string) (Message, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threadID == "" {
		threadID = s.active
	}
	t := s.find(threadID)
	if t == nil {
		return Message{}, "", false
	}
	id := s.nextMsg
	s.nextMsg++
	m := Message{ID: id, Role: role, Text: text, Time: nowSec()}
	t.Messages = append(t.Messages, m)
	if title != "" && t.Title == "New chat" {
		t.Title = title
	}
	if len(t.Messages) > 500 {
		t.Messages = t.Messages[len(t.Messages)-500:]
	}
	s.saveLocked()
	return m, t.ID, true
}

func (s *Store) RenameThread(id, title string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return false
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	if len(title) > 80 {
		title = title[:80] + "…"
	}
	t.Title = title
	s.saveLocked()
	return true
}

func (s *Store) SetPinned(id string, pinned bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return false
	}
	t.Pinned = pinned
	s.saveLocked()
	return true
}

func (s *Store) SetArchived(id string, archived bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return false
	}
	t.Archived = archived
	if archived {
		t.Pinned = false
	}
	if archived && s.active == id {
		for _, other := range s.threads {
			if other.ID != id && !other.Archived {
				s.active = other.ID
				break
			}
		}
	}
	s.saveLocked()
	return true
}

func (s *Store) DeleteThread(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	idx := -1
	for i, t := range s.threads {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	s.threads = append(s.threads[:idx], s.threads[idx+1:]...)
	if s.active == id {
		s.active = ""
		for _, t := range s.threads {
			if !t.Archived {
				s.active = t.ID
				break
			}
		}
		if s.active == "" && len(s.threads) > 0 {
			s.active = s.threads[0].ID
		}
		if s.active == "" {
			s.newThreadLocked()
			return true
		}
	}
	s.saveLocked()
	return true
}

func (s *Store) ClearThread(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.active
	}
	t := s.find(id)
	if t == nil {
		return false
	}
	t.Messages = nil
	t.SessionID = ""
	t.SessionName = "webchat-" + time.Now().Format("20060102-150405") + "-" + t.ID
	s.saveLocked()
	return true
}

func makeSnippet(text, qLower string) string {
	clean := strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
	if len(clean) <= 140 {
		return clean
	}
	lower := strings.ToLower(clean)
	idx := strings.Index(lower, qLower)
	if idx < 0 {
		return clean[:140] + "…"
	}
	start := idx - 45
	if start < 0 {
		start = 0
	}
	end := idx + len(qLower) + 75
	if end > len(clean) {
		end = len(clean)
	}
	out := clean[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(clean) {
		out += "…"
	}
	return out
}

func (s *Store) Search(query string, limit int) []SearchHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var hits []SearchHit
	for _, t := range s.threads {
		matchedMsg := false
		for i := len(t.Messages) - 1; i >= 0; i-- {
			m := t.Messages[i]
			if strings.Contains(strings.ToLower(m.Text), q) {
				hits = append(hits, SearchHit{
					ThreadID:    t.ID,
					ThreadTitle: t.Title,
					Archived:    t.Archived,
					MsgID:       m.ID,
					Role:        m.Role,
					Snippet:     makeSnippet(m.Text, q),
					Time:        m.Time,
				})
				matchedMsg = true
				if len(hits) >= limit {
					return hits
				}
			}
		}
		if !matchedMsg && strings.Contains(strings.ToLower(t.Title), q) {
			hits = append(hits, SearchHit{
				ThreadID:    t.ID,
				ThreadTitle: t.Title,
				Archived:    t.Archived,
				Snippet:     t.Title,
				Time:        lite(t).LastTime,
			})
			if len(hits) >= limit {
				return hits
			}
		}
	}
	return hits
}

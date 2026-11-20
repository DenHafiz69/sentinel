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

type Message struct {
	ID    int64        `json:"id"`
	Role  string       `json:"role"` // user | assistant | error
	Text  string       `json:"text"`
	Files []Attachment `json:"files,omitempty"`
	Time  int64        `json:"time"` // unix seconds
}

type Thread struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	SessionName string    `json:"sessionName"` // passed to hermes --continue
	SessionID   string    `json:"sessionId,omitempty"`
	Created     int64     `json:"created"`
	Messages    []Message `json:"messages"`
}

type ThreadLite struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Created  int64  `json:"created"`
	LastTime int64  `json:"lastTime"`
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
	return ThreadLite{ID: t.ID, Title: t.Title, Created: t.Created, LastTime: lt}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := UISnap{Active: s.active}
	for _, t := range s.threads {
		snap.Threads = append(snap.Threads, lite(t))
	}
	if t := s.find(s.active); t != nil {
		msgs := t.Messages
		if len(msgs) > 400 {
			msgs = msgs[len(msgs)-400:]
		}
		snap.Messages = append(snap.Messages, msgs...)
	}
	return snap
}

func (s *Store) SessionName(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(threadID); t != nil {
		return t.SessionName
	}
	return ""
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
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(s.active)
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

func (s *Store) FinalizeAssistant(threadID string, id int64, text string, files []Attachment, sessionID string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Message{ID: id, Role: "assistant", Text: text, Files: files, Time: nowSec()}
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

func (s *Store) AddError(threadID string, text string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextMsg
	s.nextMsg++
	m := Message{ID: id, Role: "error", Text: text, Time: nowSec()}
	if t := s.find(threadID); t != nil {
		t.Messages = append(t.Messages, m)
		if len(t.Messages) > 500 {
			t.Messages = t.Messages[len(t.Messages)-500:]
		}
		s.saveLocked()
	}
	return m
}

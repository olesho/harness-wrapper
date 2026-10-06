package fakeadapter

import (
	"os"
	"path/filepath"
	"sync"
)

// MaxSessions is how many Sessions of one agent the fake has open at once
// (capability concurrent_sessions). Each keeps its own directory, harness
// goroutine and cursor; what they share is kept here.
const MaxSessions = 8

// agents is what the fake's Sessions share in this process: the Session
// whose harness holds each Session directory, so that a second is refused
// session_in_use; the Sessions open in each agent, by its scratch root, which
// the breaks of Sessions side by side reach; and the opens under way in each.
var agents = struct {
	sync.Mutex
	held     map[string]*session
	open     map[string]map[*session]bool
	starting map[string]int
}{held: map[string]*session{}, open: map[string]map[*session]bool{}, starting: map[string]int{}}

// hold makes s the holder of its Session's directory, and one of its agent's
// open Sessions; false when another Session's harness holds the directory.
func (s *session) hold(dir string) bool {
	agents.Lock()
	defer agents.Unlock()
	if h := agents.held[dir]; h != nil && h != s && !s.adapter.breaks("open-twice") {
		return false
	}
	agents.held[dir] = s
	scratch := s.req.Layout.Scratch
	if agents.open[scratch] == nil {
		agents.open[scratch] = map[*session]bool{}
	}
	agents.open[scratch][s] = true
	return true
}

// letGo gives s's directory up, once its harness has exited.
func (s *session) letGo(dir string) {
	agents.Lock()
	defer agents.Unlock()
	if agents.held[dir] == s {
		delete(agents.held, dir)
	}
	scratch := s.req.Layout.Scratch
	delete(agents.open[scratch], s)
	if len(agents.open[scratch]) == 0 {
		delete(agents.open, scratch)
	}
}

// siblings are the other Sessions open in s's agent.
func (s *session) siblings() []*session {
	agents.Lock()
	defer agents.Unlock()
	var out []*session
	for o := range agents.open[s.req.Layout.Scratch] {
		if o != s {
			out = append(out, o)
		}
	}
	return out
}

// starting counts an open of s's agent as under way until done is called;
// crowded reports whether another is.
func (s *session) starting() (crowded func() bool, done func()) {
	scratch := s.req.Layout.Scratch
	agents.Lock()
	agents.starting[scratch]++
	agents.Unlock()
	crowded = func() bool {
		agents.Lock()
		defer agents.Unlock()
		return agents.starting[scratch] > 1
	}
	done = func() {
		agents.Lock()
		defer agents.Unlock()
		if agents.starting[scratch]--; agents.starting[scratch] == 0 {
			delete(agents.starting, scratch)
		}
	}
	return crowded, done
}

// workspaceRecords is every record of the Sessions kept beside s's, but its
// own: what the breaks that read across Sessions read.
func workspaceRecords(own store) []store {
	entries, err := os.ReadDir(filepath.Dir(own.dir))
	if err != nil {
		return nil
	}
	var out []store
	for _, e := range entries {
		if dir := filepath.Join(filepath.Dir(own.dir), e.Name()); e.IsDir() && dir != own.dir {
			out = append(out, store{dir: dir, adapter: own.adapter})
		}
	}
	return out
}

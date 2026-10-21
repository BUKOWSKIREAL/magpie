package sessions

import (
	"sort"
	"strconv"
	"strings"
)

func parseAntigravity(f file) *state {
	s := &state{Size: f.size, Mod: f.mod.UnixNano()}
	usages, named, cwd, err := agyRead(f)
	if err != nil {
		return &state{}
	}
	s.Cwd = cwd
	if t := title(named); t != "" {
		s.Title = t
	}
	for _, u := range usages {
		s.saw(u.at, true)
		s.use(dateOf(u.at), u.model, u.tokens)
	}
	return s
}

func antigravityCallFiles() []file { return agyFiles() }

func readAntigravityCalls(f file) (*callFile, bool) {
	st := &callFile{Agent: "agy", Path: f.path, Session: strings.TrimPrefix(f.key, "agy:")}
	usages, _, _, err := agyRead(f)
	if err != nil {
		return st, false
	}
	for _, u := range usages {
		st.Calls = append(st.Calls, Call{
			Agent:      "agy",
			Session:    st.Session,
			Model:      u.model,
			Time:       u.at,
			Tokens:     u.tokens,
			ResponseID: u.responseID,
			Msg:        st.str(st.Session + ":" + agyKey(u)),
		})
	}
	sort.SliceStable(st.Calls, func(i, j int) bool {
		if !st.Calls[i].Time.Equal(st.Calls[j].Time) {
			return st.Calls[i].Time.Before(st.Calls[j].Time)
		}
		return st.Calls[i].Msg < st.Calls[j].Msg
	})
	return st, true
}

func agyKey(u agyUsage) string {
	if u.responseID != "" {
		return "response:" + u.responseID
	}
	if u.messageID != "" {
		return "message:" + u.messageID
	}
	return "step:" + strconv.FormatInt(u.idx, 10)
}

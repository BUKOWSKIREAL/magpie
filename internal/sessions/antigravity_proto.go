package sessions

import (
	"encoding/binary"
	"errors"
	"math"
)

// Antigravity keeps each conversation as an encrypted-at-rest SQLite
// database under ~/.gemini/antigravity/conversations/<uuid>.db, but the
// metadata magpie reads is plain protobuf in two BLOB columns: a step's
// token usage in steps.metadata, and the model a generator ran in
// gen_metadata.data. Only those figures are read; no message body is
// touched. The wire format is parsed here rather than through a generated
// message class, as devin_images.go does for its own vendor.
//
// Field numbers, taken from the real databases and cc-bar's reader:
//
//	steps.metadata (StepMetadata)
//	  1  Timestamp     the step's time (google.protobuf.Timestamp)
//	  9  ModelCall     a model call's statistics (StepModelCall)
//	    2  input       the uncached input tokens (cache is apart)
//	    3  output      the output tokens (thinking included)
//	    4  cacheWrite  tokens written to the cache
//	    5  cacheRead   tokens read from the cache
//	    7  messageId   the message the call belongs to
//	    11 responseId  the response the call is one step of
//
//	gen_metadata.data (GeneratorMetadata)
//	  1  ChatMessage   the message asked of the model (LLMChat)
//	    19 model       the model id as the generator named it
//	    21 model       alias, when 19 is absent
//	    22 model       name, when 19 and 21 are absent
//	  2  stepIndices   the steps this generator owns (repeated, packed)

// errAntigravityMetadata ends a walk of a malformed protobuf.
var errAntigravityMetadata = errors.New("antigravity: malformed metadata")

// agField is one protobuf field read from a message.
type agField struct {
	num    int
	varint uint64
	bytes  []byte
	hasVar bool
}

// agReader walks a protobuf message's fields, carrying nothing but the
// bytes and an offset.
type agReader struct {
	b   []byte
	off int
}

func (r *agReader) atEnd() bool { return r.off >= len(r.b) }

func (r *agReader) varint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		return 0, errAntigravityMetadata
	}
	r.off += n
	return v, nil
}

// next reads one field, or reports the end of the message. Groups (wire
// type 3/4) and unknown scalar widths are skipped as their wire type says.
func (r *agReader) next() (agField, bool, error) {
	if r.atEnd() {
		return agField{}, false, nil
	}
	tag, err := r.varint()
	if err != nil {
		return agField{}, false, err
	}
	num, wire := int(tag>>3), int(tag&7)
	if num <= 0 {
		return agField{}, false, errAntigravityMetadata
	}
	switch wire {
	case 0:
		v, err := r.varint()
		if err != nil {
			return agField{}, false, err
		}
		return agField{num: num, varint: v, hasVar: true}, true, nil
	case 1:
		if len(r.b)-r.off < 8 {
			return agField{}, false, errAntigravityMetadata
		}
		r.off += 8
		return agField{num: num}, true, nil
	case 2:
		l, err := r.varint()
		if err != nil {
			return agField{}, false, err
		}
		if l > uint64(len(r.b)-r.off) {
			return agField{}, false, errAntigravityMetadata
		}
		v := r.b[r.off : r.off+int(l)]
		r.off += int(l)
		return agField{num: num, bytes: v}, true, nil
	case 5:
		if len(r.b)-r.off < 4 {
			return agField{}, false, errAntigravityMetadata
		}
		r.off += 4
		return agField{num: num}, true, nil
	default:
		return agField{}, false, errAntigravityMetadata
	}
}

// agText is a field's bytes as trimmed text, "" when empty.
func agText(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// agTimestamp reads a google.protobuf.Timestamp's seconds and nanos.
func agTimestamp(b []byte) (int64, int32, error) {
	r := &agReader{b: b}
	var sec int64
	var nanos int64
	for {
		f, ok, err := r.next()
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			break
		}
		if !f.hasVar {
			continue
		}
		switch f.num {
		case 1:
			sec = int64(f.varint)
		case 2:
			nanos = int64(f.varint)
		}
	}
	if nanos <= -1e9 || nanos >= 1e9 || sec < -62135596800 || sec > 253402300799 {
		return 0, 0, errAntigravityMetadata
	}
	return sec, int32(nanos), nil
}

// agStepUsage is the token usage one step records.
type agStepUsage struct {
	at         int64 // unix seconds, 0 when the step names none
	nanos      int32
	input      int
	output     int
	cacheWrite int
	cacheRead  int
	messageID  string
	responseID string
}

// hasTokens is whether the step recorded any usage at all.
func (u agStepUsage) hasTokens() bool {
	return u.input > 0 || u.output > 0 || u.cacheWrite > 0 || u.cacheRead > 0
}

// agParseStep reads a steps.metadata row: the step's timestamp and the
// model call's token figures.
func agParseStep(b []byte) (agStepUsage, error) {
	var u agStepUsage
	r := &agReader{b: b}
	for {
		f, ok, err := r.next()
		if err != nil {
			return u, err
		}
		if !ok {
			break
		}
		switch f.num {
		case 1:
			sec, nanos, err := agTimestamp(f.bytes)
			if err != nil {
				return u, err
			}
			u.at, u.nanos = sec, nanos
		case 9:
			if err := agParseModelCall(f.bytes, &u); err != nil {
				return u, err
			}
		}
	}
	return u, nil
}

// agParseModelCall reads a StepModelCall's statistics into u.
func agParseModelCall(b []byte, u *agStepUsage) error {
	r := &agReader{b: b}
	for {
		f, ok, err := r.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		switch {
		case f.hasVar:
			v, err := agToken(f.varint)
			if err != nil {
				return err
			}
			switch f.num {
			case 2:
				u.input = v
			case 3:
				u.output = v
			case 4:
				u.cacheWrite = v
			case 5:
				u.cacheRead = v
			}
		case f.bytes != nil:
			switch f.num {
			case 7:
				u.messageID = agText(f.bytes)
			case 11:
				u.responseID = agText(f.bytes)
			}
		}
	}
}

// agGenerator is a gen_metadata row: the model it ran and the steps it
// owns.
type agGenerator struct {
	model   string
	indices []int64
}

// agParseGenerator reads a gen_metadata.data row.
func agParseGenerator(b []byte) (agGenerator, error) {
	var g agGenerator
	r := &agReader{b: b}
	for {
		f, ok, err := r.next()
		if err != nil {
			return g, err
		}
		if !ok {
			return g, nil
		}
		switch f.num {
		case 1:
			if m := agChatModel(f.bytes); m != "" {
				g.model = m
			}
		case 2:
			idx, err := agPackedInts(f, f.bytes)
			if err != nil {
				return g, err
			}
			g.indices = append(g.indices, idx...)
		}
	}
}

// agChatModel reads the model a generator's chat message names: field 19,
// else 21, else 22.
func agChatModel(b []byte) string {
	r := &agReader{b: b}
	var at19, at21, at22 string
	for {
		f, ok, err := r.next()
		if err != nil {
			return ""
		}
		if !ok {
			break
		}
		if f.bytes == nil {
			continue
		}
		switch f.num {
		case 19:
			at19 = agText(f.bytes)
		case 21:
			at21 = agText(f.bytes)
		case 22:
			at22 = agText(f.bytes)
		}
	}
	if at19 != "" {
		return at19
	}
	if at21 != "" {
		return at21
	}
	return at22
}

// agPackedInts reads a repeated int64 field, packed or one at a time.
func agPackedInts(f agField, b []byte) ([]int64, error) {
	if f.hasVar {
		if f.varint > math.MaxInt64 {
			return nil, errAntigravityMetadata
		}
		return []int64{int64(f.varint)}, nil
	}
	r := &agReader{b: b}
	var out []int64
	for !r.atEnd() {
		v, err := r.varint()
		if err != nil {
			return nil, err
		}
		if v > math.MaxInt64 {
			return nil, errAntigravityMetadata
		}
		out = append(out, int64(v))
	}
	return out, nil
}

// agToken narrows a token count, refusing a value an int cannot hold.
func agToken(v uint64) (int, error) {
	if v > math.MaxInt64 {
		return 0, errAntigravityMetadata
	}
	return int(v), nil
}

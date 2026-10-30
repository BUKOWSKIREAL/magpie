package usage

import (
	"testing"
	"time"
)

// The fork's response-ID match must run before the upstream token/time
// fallback, and each gateway call can account for only one local call.
func TestAntigravityResponseIDMatchingAfterRebase(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, packed := range []bool{false, true} {
		for _, newest := range []bool{false, true} {
			gateway, local := &rowChunk{}, &rowChunk{}
			gateway.add(Row{Record: Record{Time: now, Agent: "agy", Provider: "relay", ResponseID: "response-1", Input: 10, Output: 2, Status: 200}}, "", 0, false)
			gateway.add(Row{Record: Record{Time: now, Agent: "agy", Provider: "relay", Input: 20, Output: 3, Status: 200}}, "", 1, false)
			// A response ID identifies the call even when the local session,
			// time and token counts differ from the gateway's record.
			for i := range 2 {
				local.add(Row{Record: Record{Time: now.Add(time.Duration(i+1) * time.Hour), Agent: "agy", Session: "desktop", ResponseID: "response-1", Input: 99, Output: 9}, Source: "log"}, "", int64(i), false)
			}
			// The gateway call already matched by response ID cannot also
			// hide an unrelated local call with the same token counts.
			local.add(Row{Record: Record{Time: now, Agent: "agy", Session: "direct", Input: 10, Output: 2}, Source: "log"}, "", 2, false)
			// Calls without response IDs still use upstream's fallback.
			local.add(Row{Record: Record{Time: now, Agent: "agy", Session: "cli", Input: 20, Output: 3}, Source: "log"}, "", 3, false)
			local.serial = nextChunkSerial()
			if packed {
				local = local.pack()
				if len(local.Archive) == 0 {
					t.Fatal("fixture was not packed")
				}
			}
			got := matchedBlocks([]*rowChunk{gateway}, []*rowChunk{local}, nil, nil, time.Time{}, newest)
			want := 0
			if newest {
				want = 1
			}
			if got.count() != 2 || !got.has(rowRef{local, want}) || !got.has(rowRef{local, 3}) {
				t.Fatalf("packed=%v newest=%v: matched %v; want response row %d and fallback row 3", packed, newest, got, want)
			}
		}
	}
}

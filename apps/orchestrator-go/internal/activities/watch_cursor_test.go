package activities

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSeenObjectsMergesLegacyAndScheduleCursors(t *testing.T) {
	cfg := map[string]any{"cursor": map[string]any{
		"seen": map[string]any{"a.csv": "1"},
		"bySchedule": map[string]any{
			"7": map[string]any{"seen": map[string]any{"b.csv": "2"}},
			"8": map[string]any{"seen": map[string]any{"c.csv": "3"}},
		},
	}}
	require.Equal(t, map[string]string{"a.csv": "1", "b.csv": "2"}, SeenObjects(cfg, 7))
	require.Equal(t, map[string]string{"a.csv": "1", "c.csv": "3"}, SeenObjects(cfg, 8))
	require.Equal(t, map[string]string{"a.csv": "1"}, SeenObjects(cfg, 9), "a new schedule inherits only the legacy cursor")
	require.Equal(t, map[string]string{"a.csv": "1"}, SeenObjects(cfg, 0))
	require.Empty(t, SeenObjects(map[string]any{}, 7))
}

func TestAdvanceSeenIsScopedToTheSchedule(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	cursor := advanceSeen(nil, 7, map[string]string{"x.csv": "e1"}, now)
	cursor = advanceSeen(cursor, 8, map[string]string{"y.csv": "e2"}, now)
	cursor = advanceSeen(cursor, 0, map[string]string{"legacy.csv": "e0"}, now)

	cfg := map[string]any{"cursor": cursor}
	require.Equal(t, map[string]string{"legacy.csv": "e0", "x.csv": "e1"}, SeenObjects(cfg, 7))
	require.Equal(t, map[string]string{"legacy.csv": "e0", "y.csv": "e2"}, SeenObjects(cfg, 8))
	require.Equal(t, "2026-10-19T00:00:00Z", cursor["updatedAt"])

	// Advancing one schedule never touches another's progress.
	cursor = advanceSeen(cursor, 7, map[string]string{"x.csv": "e1-changed"}, now)
	require.Equal(t, "e1-changed", SeenObjects(map[string]any{"cursor": cursor}, 7)["x.csv"])
	require.Equal(t, map[string]string{"legacy.csv": "e0", "y.csv": "e2"}, SeenObjects(map[string]any{"cursor": cursor}, 8))
}

func TestAdvanceSeenBoundsEachCursor(t *testing.T) {
	newly := map[string]string{}
	for i := 0; i < 10_050; i++ {
		newly[string(rune('a'))+padInt(i)] = "e"
	}
	cursor := advanceSeen(nil, 3, newly, time.Now())
	require.Len(t, SeenObjects(map[string]any{"cursor": cursor}, 3), 10_000)
}

func padInt(i int) string {
	s := ""
	for _, c := range []byte{byte('0' + i/10000%10), byte('0' + i/1000%10), byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)} {
		s += string(c)
	}
	return s
}

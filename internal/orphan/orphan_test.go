package orphan

import (
	"testing"
	"time"
)

func TestIsOrphanedAt(t *testing.T) {
	const threshold = 180 * 24 * time.Hour
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		signal ActivitySignal
		want   bool
	}{
		{
			name: "recently active via commit is not orphaned",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-5 * 24 * time.Hour),
			},
			want: false,
		},
		{
			name: "recently active via PR review only is not orphaned",
			signal: ActivitySignal{
				LastPRReviewAt: now.Add(-1 * time.Hour),
			},
			want: false,
		},
		{
			name: "200 days of silence with a 180 day threshold is orphaned",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-200 * 24 * time.Hour),
			},
			want: true,
		},
		{
			name: "most recent of two stale signals still governs and is orphaned",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-400 * 24 * time.Hour),
				LastPRReviewAt:    now.Add(-190 * 24 * time.Hour),
			},
			want: true,
		},
		{
			name: "a recent PR review rescues an old last commit",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-400 * 24 * time.Hour),
				LastPRReviewAt:    now.Add(-1 * 24 * time.Hour),
			},
			want: false,
		},
		{
			name:   "no activity ever recorded is orphaned",
			signal: ActivitySignal{},
			want:   true,
		},
		{
			name: "activity exactly at the threshold is not yet orphaned",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-threshold),
			},
			want: false,
		},
		{
			name: "activity one second past the threshold is orphaned",
			signal: ActivitySignal{
				LastHumanCommitAt: now.Add(-threshold - time.Second),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOrphanedAt(tt.signal, threshold, now)
			if got != tt.want {
				t.Errorf("IsOrphanedAt(%+v, %v, %v) = %v, want %v", tt.signal, threshold, now, got, tt.want)
			}
		})
	}
}

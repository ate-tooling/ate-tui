package explain

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ActorState renders an ActorState the way the UI shows it.
func ActorState(s ateapi.ActorState) string {
	return title(trimEnum(s.String(), "ACTOR_STATE_"))
}

// WorkerState renders a WorkerState the way the UI shows it.
func WorkerState(s ateapi.WorkerState) string {
	return title(trimEnum(s.String(), "WORKER_STATE_"))
}

// SnapshotFidelity renders a snapshot's fidelity the way the UI shows it,
// "-" when unset.
func SnapshotFidelity(f ateapi.SnapshotFidelity) string {
	return orDash(title(enumValue(f.String(), "SNAPSHOT_FIDELITY_")))
}

// TagScope renders a tag's scope the way the UI shows it, "-" when unset.
func TagScope(s ateapi.TagScope) string {
	return orDash(title(enumValue(s.String(), "TAG_SCOPE_")))
}

func trimEnum(s, prefix string) string { return strings.TrimPrefix(s, prefix) }

// enumValue is trimEnum with UNSPECIFIED, the proto zero value, as "".
func enumValue(s, prefix string) string {
	if s = trimEnum(s, prefix); s == "UNSPECIFIED" {
		return ""
	}
	return s
}

func title(s string) string {
	if s == "" {
		return s
	}
	return s[:1] + strings.ToLower(s[1:])
}

func refString(r *ateapi.ObjectRef) string {
	if r.GetAtespace() == "" {
		return r.GetName()
	}
	return r.GetAtespace() + "/" + r.GetName()
}

func runtimeString(r *ateapi.ActorSandboxRuntime) string {
	return r.GetSandboxClass()
}

func labelsString(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ago renders a protobuf timestamp as "5m ago", or "-" when unset.
func ago(t *timestamppb.Timestamp, now time.Time) string {
	if t == nil {
		return "-"
	}
	a := Age(t.AsTime(), now)
	if a == "-" {
		return a
	}
	return a + " ago"
}

// Age renders a duration like kubectl: 5s, 3m, 2h, 4d.
func Age(t, now time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "-"
	}
	return roundDur(now.Sub(t))
}

func roundDur(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Millicores renders CPU like Kubernetes quantities.
func Millicores(m int64) string {
	switch {
	case m == 0:
		return "-"
	case m%1000 == 0:
		return fmt.Sprintf("%d", m/1000)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// Bytes renders memory in binary units.
func Bytes(b int64) string {
	if b == 0 {
		return "-"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	// Pick the unit after rounding to one decimal, so 1048575 is 1Mi,
	// not 1024.0Ki.
	v, exp := float64(b)/unit, 0
	for math.Round(v*10)/10 >= unit && exp < 5 {
		v /= unit
		exp++
	}
	v = math.Round(v*10) / 10
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d%ci", int64(v), "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f%ci", v, "KMGTPE"[exp])
}

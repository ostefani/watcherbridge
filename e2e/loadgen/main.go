// Command loadgen drives open-loop signed webhook workloads for Step 31
// load measurements: a burst (all N released at once across -workers
// senders) or a sustained rate (ticker-fed queue at -rate events/s for
// -duration). The loop never slows when responses do: sustained mode
// drops (and counts) arrivals the pool cannot take, so offered load is
// explicit and scheduling delay is reported, never silent.
//
// Bodies come from -fixtures (JSON files, {{NOW}} expanded, envelope
// rewritten to the per-request identity exactly like e2e/send), cycling
// -refs distinct synthetic references so subject cardinality is real.
// Results stream as JSONL to -out; a summary line goes to stdout.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostefani/watcherbridge/e2e/sig"
)

func main() {
	url := flag.String("url", "", "webhook URL (required)")
	secret := flag.String("secret", "", "HMAC secret (required)")
	mode := flag.String("mode", "burst", "burst|sustained")
	n := flag.Int("n", 512, "burst events")
	rate := flag.Float64("rate", 50, "sustained events/s")
	duration := flag.Duration("duration", 60*time.Second, "sustained duration")
	workers := flag.Int("workers", 16, "parallel senders")
	fixtures := flag.String("fixtures", "", "comma-separated JSON fixture paths (required)")
	prefix := flag.String("prefix", "load", "event id prefix")
	out := flag.String("out", "", "JSONL results path (required)")
	flag.Parse()
	if *url == "" || *secret == "" || *fixtures == "" || *out == "" {
		log.Fatal("url, secret, fixtures, and out are required")
	}
	bodies := loadFixtures(strings.Split(*fixtures, ","))
	client := &http.Client{Timeout: 30 * time.Second}
	type result struct {
		ID     string  `json:"event_id"`
		SHA256 string  `json:"body_sha256"`
		Tms    int64   `json:"t_ms"`
		Status int     `json:"status"`
		LatMs  float64 `json:"lat_ms"`
	}
	outf, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer outf.Close()
	var mu sync.Mutex
	var sent, ok202, rejected, failed, dropped, offered int64
	var lats []float64
	emit := func(id, body string, status int, lat time.Duration) {
		sum := sha256.Sum256([]byte(body))
		rec, _ := json.Marshal(result{ID: id, SHA256: hex.EncodeToString(sum[:]), Tms: time.Now().UnixMilli(), Status: status, LatMs: float64(lat.Microseconds()) / 1000})
		mu.Lock()
		fmt.Fprintln(outf, string(rec))
		lats = append(lats, float64(lat.Microseconds())/1000)
		mu.Unlock()
		atomic.AddInt64(&sent, 1)
		switch {
		case status == 202:
			atomic.AddInt64(&ok202, 1)
		case status >= 400 && status < 500:
			atomic.AddInt64(&rejected, 1)
		case status < 0:
			atomic.AddInt64(&failed, 1)
		default:
			atomic.AddInt64(&failed, 1)
		}
	}
	// Job i carries a unique identity and a rotating reference so
	// subject cardinality is real, not one hot anchor.
	job := func(i int) (id, key, typ, body string) {
		base := bodies[i%len(bodies)]
		id = fmt.Sprintf("%s-%06d", *prefix, i)
		typ = base.typ
		key = fmt.Sprintf("load-key-%04d", i%64)
		var env map[string]any
		if err := json.Unmarshal([]byte(base.text), &env); err != nil {
			log.Fatal(err)
		}
		env["event_id"], env["event_key"], env["event_type"] = id, key, typ
		if host, ok := env["host"].(map[string]any); ok {
			stampRef(host, i)
		}
		for _, view := range []string{"previous", "current"} {
			if v, ok := env[view].(map[string]any); ok {
				stampRef(v, i)
			}
		}
		raw, err := json.Marshal(env)
		if err != nil {
			log.Fatal(err)
		}
		return id, key, typ, string(raw)
	}
	send := func(i int) {
		id, key, typ, body := job(i)
		status, lat, err := sig.Post(client, *url, *secret, id, key, typ, sig.Timestamp(), body)
		if err != nil {
			emit(id, body, -1, lat)
			return
		}
		emit(id, body, status, lat)
	}
	switch *mode {
	case "burst":
		offered = int64(*n)
		var wg sync.WaitGroup
		slots := make(chan int, *n)
		for i := 0; i < *n; i++ {
			slots <- i
		}
		close(slots)
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range slots {
					send(i)
				}
			}()
		}
		wg.Wait()
	case "sustained":
		tick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
		defer tick.Stop()
		deadline := time.Now().Add(*duration)
		slots := make(chan int, *workers*2)
		var wg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range slots {
					send(i)
				}
			}()
		}
		i := 0
		for time.Now().Before(deadline) {
			<-tick.C
			offered++
			select {
			case slots <- i:
			default:
				atomic.AddInt64(&dropped, 1) // pool saturated: count it, never slow the clock
			}
			i++
		}
		close(slots)
		wg.Wait()
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
	sort.Slice(lats, func(a, b int) bool { return lats[a] < lats[b] })
	perc := func(p float64) float64 {
		if len(lats) == 0 {
			return -1
		}
		k := int(p * float64(len(lats)-1))
		return lats[k]
	}
	summary, _ := json.Marshal(map[string]any{
		"offered": offered, "sent": sent, "accepted_202": ok202, "rejected_4xx": rejected,
		"failed": failed, "dropped_unsent": atomic.LoadInt64(&dropped),
		"lat_ms_p50": perc(0.50), "lat_ms_p99": perc(0.99), "lat_ms_max": perc(1.0),
	})
	fmt.Println(string(summary))
}

type fixture struct {
	typ  string
	text string
}

func loadFixtures(paths []string) []fixture {
	var out []fixture
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			log.Fatal(err)
		}
		text := strings.ReplaceAll(string(raw), "{{NOW}}", now)
		var env struct {
			Type string `json:"event_type"`
		}
		if err := json.Unmarshal([]byte(text), &env); err != nil {
			log.Fatal(err)
		}
		out = append(out, fixture{typ: env.Type, text: text})
	}
	return out
}

// stampRef rotates the host identity across 64 synthetic references so
// the workload has real subject cardinality and churn.
func stampRef(view map[string]any, i int) {
	k := i % 64
	mac := fmt.Sprintf("02:53:%02x:%02x:%02x:%02x", k, i>>6&0xff, i&0xff, k^0xaa)
	view["mac"] = mac
	view["ip"] = fmt.Sprintf("198.51.100.%d", k+1)
	view["host_id"] = "mac:" + mac
	if src, ok := view["identity_source"]; ok && src == "ip" {
		view["host_id"] = fmt.Sprintf("ip:198.51.100.%d", k+1)
	}
}

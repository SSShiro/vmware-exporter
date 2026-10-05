package vmware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/types"
	"vmware-exporter/internal/config"
	"vmware-exporter/pkg/logging"
)

func TestParseTimeout(t *testing.T) {
	def := 60 * time.Second
	cases := map[string]time.Duration{
		"":       def,
		"30s":    30 * time.Second, // used to be multiplied by time.Second again
		"30":     30 * time.Second,
		"1m":     time.Minute,
		"junk":   def,
		"-5":     def,
		"0":      def,
		" 15 ":   15 * time.Second,
		"1500ms": 1500 * time.Millisecond,
	}
	for in, want := range cases {
		if got := parseTimeout(in, def); got != want {
			t.Errorf("parseTimeout(%q)=%v, want %v", in, got, want)
		}
	}
}

func TestSensorHealthOfNil(t *testing.T) {
	if got := sensorHealthOf(nil); got != 0 {
		t.Fatalf("nil health must map to 0, got %v", got)
	}
	if got := sensorHealthOf(&types.ElementDescription{Description: types.Description{}, Key: "Green"}); got != 1 {
		t.Fatalf("green must map to 1, got %v", got)
	}
}

// Probes of unreachable targets must fail without panics/exit, run concurrently and
// must not replace the global prometheus registry.
func TestProbeConcurrentAndGlobalRegistry(t *testing.T) {
	t.Setenv("VMWARE_EXPORTER_SCRAPE_TIMEOUT", "2s")
	logger := logging.GetLogger()
	h := GetHandler(&logger, config.GetConfig(), nil).(*exporterHandler)

	before := prometheus.DefaultGatherer
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/probe?target=127.0.0.1:1", nil)
			h.probe(rec, req)
			if rec.Code == 0 {
				t.Error("no response")
			}
		}()
	}
	wg.Add(1)
	go func() { // reload races with probes
		defer wg.Done()
		h.reload(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/-/reload", nil))
	}()
	wg.Wait()

	if prometheus.DefaultGatherer != before {
		t.Fatal("probe replaced prometheus.DefaultGatherer")
	}
}

type panicService struct{ msg string }

func (p panicService) statuses() ([]*Status, error) { panic(p.msg) }
func (p panicService) error(error)                  {}

// A panic inside Collect runs outside of net/http's recover and used to kill the process.
func TestCollectRecoversFromPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(panicService{"boom"}))
	if _, err := reg.Gather(); err == nil {
		t.Fatal("expected gather error, got nil")
	}
}

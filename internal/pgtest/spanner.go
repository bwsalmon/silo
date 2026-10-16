package pgtest

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// SpannerEnv names the environment variable that points Start at Cloud
// Spanner's emulator, through PGAdapter, instead of a PostgreSQL of its
// own (docs/spanner-evaluation.md). It is "1", for PGAdapter on
// 127.0.0.1:5433, or PGAdapter's host:port. PGAdapter must run with
// -r autoConfigEmulator=true, as the pgadapter-emulator image does, so
// that connecting to a database that does not exist creates it:
//
//	docker run -d -p 127.0.0.1:5433:5432 gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator
//	SILO_TEST_SPANNER=1 go test ./hub/...
const SpannerEnv = "SILO_TEST_SPANNER"

// DefaultSpannerAddr is where SpannerEnv=1 looks for PGAdapter.
const DefaultSpannerAddr = "127.0.0.1:5433"

// SpannerRESTEnv names the environment variable holding the emulator's
// REST address, through which each test's database is dropped when the
// test ends: an emulator instance holds no more than 100. It defaults to
// DefaultSpannerREST.
const SpannerRESTEnv = "SILO_TEST_SPANNER_REST"

// DefaultSpannerREST is the emulator's REST port, published by the
// pgadapter-emulator image as 9020.
const DefaultSpannerREST = "127.0.0.1:9020"

// The emulator project and instance PGAdapter's autoConfigEmulator
// creates databases in, as the pgadapter-emulator image starts it.
const spannerInstance = "projects/emulator-project/instances/test-instance"

// Spanner reports whether SpannerEnv is set.
func Spanner() bool {
	v := os.Getenv(SpannerEnv)
	return v != "" && v != "0" && v != "false"
}

func spannerAddr() string {
	v := os.Getenv(SpannerEnv)
	if _, _, err := net.SplitHostPort(v); err == nil {
		return v
	}
	return DefaultSpannerAddr
}

// StartSpanner gives t a database of its own on the Spanner emulator,
// reached through PGAdapter on the PostgreSQL wire protocol, and fails
// t when PGAdapter does not answer. The emulator keeps it in memory
// until t ends, when it is dropped.
func StartSpanner(t testing.TB) *Server {
	t.Helper()
	addr := spannerAddr()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("%s is set but PGAdapter does not answer on %s: %v", SpannerEnv, addr, err)
	}
	c.Close()
	// Spanner database IDs are 2-30 characters, lower case, starting
	// with a letter.
	b := make([]byte, 12)
	rand.Read(b)
	db := "t" + hex.EncodeToString(b)
	t.Cleanup(func() { dropSpanner(t, db) })
	_, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return &Server{
		URL:     fmt.Sprintf("postgres://postgres@%s/%s?sslmode=disable", addr, db),
		Port:    p,
		Spanner: true,
	}
}

// dropSpanner drops the database db through the emulator's REST API,
// once every connection to it has closed. A failure is logged, not
// failed: it costs only one of the instance's 100 databases.
func dropSpanner(t testing.TB, db string) {
	rest := cmp.Or(os.Getenv(SpannerRESTEnv), DefaultSpannerREST)
	url := "http://" + rest + "/v1/" + spannerInstance + "/databases/" + db
	var last string
	for range 20 {
		req, _ := http.NewRequest(http.MethodDelete, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
				return
			}
			last = resp.Status + ": " + string(body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("pgtest: dropping Spanner database %s: %s", db, last)
}

// SkipOnSpanner skips t, saying why, when SpannerEnv is set: for a test
// of something Spanner does not have at all (roles and GRANT, LISTEN
// and NOTIFY, pg_dump), which docs/spanner-evaluation.md lists.
func SkipOnSpanner(t testing.TB, why string) {
	t.Helper()
	if Spanner() {
		t.Skipf("not on Spanner: %s (docs/spanner-evaluation.md)", why)
	}
}

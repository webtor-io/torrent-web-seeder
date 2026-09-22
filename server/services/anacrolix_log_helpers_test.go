package services

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type panickingHandler struct{}

func (panickingHandler) ServeHTTP(http.ResponseWriter, *http.Request) { panic("0 1869") }

type statusRecorder struct {
	*httptest.ResponseRecorder
	status int
}

func newTestResponseRecorder() *statusRecorder {
	return &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
}
func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseRecorder.WriteHeader(c) }
func newTestRequest() *http.Request         { return httptest.NewRequest(http.MethodGet, "/x", nil) }

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

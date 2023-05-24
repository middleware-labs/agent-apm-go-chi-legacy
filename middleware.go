package otelchi

import (
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	otelcontrib "go.opentelemetry.io/contrib"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/middleware-labs/agent-apm-go-chi-legacy"
)

// Middleware sets up a handler to start tracing the incoming
// requests. The serverName parameter should describe the name of the
// (virtual) server handling the request.
func Middleware(serverName string, opts ...Option) func(next http.Handler) http.Handler {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		tracerName,
		oteltrace.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	return func(handler http.Handler) http.Handler {
		return traceware{
			serverName:          serverName,
			tracer:              tracer,
			propagators:         cfg.Propagators,
			handler:             handler,
			chiRoutes:           cfg.ChiRoutes,
			reqMethodInSpanName: cfg.RequestMethodInSpanName,
			filter:              cfg.Filter,
		}
	}
}

type traceware struct {
	serverName          string
	tracer              oteltrace.Tracer
	propagators         propagation.TextMapPropagator
	handler             http.Handler
	chiRoutes           chi.Routes
	reqMethodInSpanName bool
	filter              func(r *http.Request) bool
}

type recordingResponseWriter struct {
	writer  http.ResponseWriter
	written bool
	status  int
}

var rrwPool = &sync.Pool{
	New: func() interface{} {
		return &recordingResponseWriter{}
	},
}

func getRRW(writer http.ResponseWriter) *recordingResponseWriter {
	rrw := rrwPool.Get().(*recordingResponseWriter)
	rrw.written = false
	rrw.status = 0
	rrw.writer = httpsnoop.Wrap(writer, httpsnoop.Hooks{
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) {
				if !rrw.written {
					rrw.written = true
					rrw.status = http.StatusOK
				}
				return next(b)
			}
		},
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(statusCode int) {
				if !rrw.written {
					rrw.written = true
					rrw.status = statusCode
				}
				next(statusCode)
			}
		},
	})
	return rrw
}

func putRRW(rrw *recordingResponseWriter) {
	rrw.writer = nil
	rrwPool.Put(rrw)
}

// ServeHTTP implements the http.Handler interface. It does the actual
// tracing of the request.
func (tw traceware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// skip if filter returns false
	if tw.filter != nil && !tw.filter(r) {
		tw.handler.ServeHTTP(w, r)
		return
	}

	// extract tracing header using propagator
	ctx := tw.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	// create span, based on specification, we need to set already known attributes
	// when creating the span, the only thing missing here is HTTP route pattern since
	// in go-chi/chi route pattern could only be extracted once the request is executed
	// check here for details:
	//
	// https://github.com/go-chi/chi/issues/150#issuecomment-278850733
	//
	// if we have access to chi routes, we could extract the route pattern beforehand.
	spanName := ""
	routePattern := ""
	if tw.chiRoutes != nil {
		rctx := chi.NewRouteContext()
		if tw.chiRoutes.Match(rctx, r.Method, r.URL.Path) {
			routePattern = rctx.RoutePattern()
			spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		}
	}

	ctx, span := tw.tracer.Start(
		ctx, spanName,
		oteltrace.WithAttributes(semconv.NetAttributesFromHTTPRequest("tcp", r)...),
		oteltrace.WithAttributes(semconv.EndUserAttributesFromHTTPRequest(r)...),
		oteltrace.WithAttributes(semconv.HTTPServerAttributesFromHTTPRequest(tw.serverName, routePattern, r)...),
		oteltrace.WithSpanKind(oteltrace.SpanKindServer),
	)
	defer span.End()

	// get recording response writer
	rrw := getRRW(w)
	defer putRRW(rrw)

	// execute next http handler
	r = r.WithContext(ctx)
	tw.handler.ServeHTTP(rrw.writer, r)

	// set span name & http route attribute if necessary
	if len(routePattern) == 0 {
		routePattern = chi.RouteContext(r.Context()).RoutePattern()
		span.SetAttributes(semconv.HTTPRouteKey.String(routePattern))

		spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		span.SetName(spanName)
	}

	// set status code attribute
	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(rrw.status))

	// set span status
	spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCode(rrw.status)
	span.SetStatus(spanStatus, spanMessage)
}

func addPrefixToSpanName(shouldAdd bool, prefix, spanName string) string {
	// in chi v5.0.8, the root route will be returned has an empty string
	// (see github.com/go-chi/chi/v5@v5.0.8/context.go:126)
	if spanName == "" {
		spanName = "/"
	}

	if shouldAdd && len(spanName) > 0 {
		spanName = prefix + " " + spanName
	}
	return spanName
}

// config is used to configure the mux middleware.
type config struct {
	TracerProvider          oteltrace.TracerProvider
	Propagators             propagation.TextMapPropagator
	ChiRoutes               chi.Routes
	RequestMethodInSpanName bool
	Filter                  func(r *http.Request) bool
}

// Option specifies instrumentation configuration options.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithPropagators specifies propagators to use for extracting
// information from the HTTP requests. If none are specified, global
// ones will be used.
func WithPropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		cfg.Propagators = propagators
	})
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
func WithTracerProvider(provider oteltrace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		cfg.TracerProvider = provider
	})
}

// WithChiRoutes specified the routes that being used by application. Its main
// purpose is to provide route pattern as span name during span creation. If this
// option is not set, by default the span will be given name at the end of span
// execution. For some people, this behavior is not desirable since they want
// to override the span name on underlying handler. By setting this option, it
// is possible for them to override the span name.
func WithChiRoutes(routes chi.Routes) Option {
	return optionFunc(func(cfg *config) {
		cfg.ChiRoutes = routes
	})
}

// WithRequestMethodInSpanName is used for adding http request method to span name.
// While this is not necessary for vendors that properly implemented the tracing
// specs (e.g Jaeger, AWS X-Ray, etc...), but for other vendors such as Elastic
// and New Relic this might be helpful.
//
// See following threads for details:
//
// - https://github.com/riandyrn/otelchi/pull/3#issuecomment-1005883910
// - https://github.com/riandyrn/otelchi/issues/6#issuecomment-1034461912
func WithRequestMethodInSpanName(isActive bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.RequestMethodInSpanName = isActive
	})
}

// WithFilter is used for filtering request that should not be traced.
// This is useful for filtering health check request, etc.
// A Filter must return true if the request should be traced.
func WithFilter(filter func(r *http.Request) bool) Option {
	return optionFunc(func(cfg *config) {
		cfg.Filter = filter
	})
}

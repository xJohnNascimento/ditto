package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/google/uuid"

	"github.com/xJohnNascimento/ditto/internal/config"
	"github.com/xJohnNascimento/ditto/internal/provider"
)

type Config struct {
	Service   *provider.Service
	Accounts  []config.Account
	AdminKey  string
	Profile   string
	DatasetID string
}

type server struct {
	cfg      Config
	accounts map[[32]byte]string
	adminKey [32]byte
}

const maxBodyBytes = 1 << 20

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// NewHandler routes every operation of the contract behind the security it declares: provider
// operations authenticate an account, spend its request budget and are recorded; admin operations
// take the separate admin key. Every request is validated against the contract before its handler.
func NewHandler(spec []byte, cfg Config) (http.Handler, error) {
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("load contract: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validate contract: %w", err)
	}
	s := &server{cfg: cfg, accounts: make(map[[32]byte]string), adminKey: sha256.Sum256([]byte(cfg.AdminKey))}
	for _, a := range cfg.Accounts {
		s.accounts[sha256.Sum256([]byte(a.Key))] = a.ID
	}
	handlers := s.handlers()

	mux := http.NewServeMux()
	allowed := make(map[string][]string)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			route := &routers.Route{Spec: doc, Path: path, PathItem: item, Method: method, Operation: op}
			handler, ok := handlers[op.OperationID]
			if !ok {
				return nil, fmt.Errorf("operation %s has no handler", op.OperationID)
			}
			switch scheme(doc, op) {
			case "providerKey":
				handler = s.provider(route, handler)
			case "adminKey":
				handler = s.admin(route, handler)
			}
			mux.HandleFunc(method+" "+path, withOperation(op.OperationID, handler))
			allowed[path] = append(allowed[path], method)
		}
	}
	// A pattern without a method is less specific than every "METHOD path" above, so it only sees
	// the methods the contract does not declare; "/" sees every path it does not declare.
	for path, methods := range allowed {
		slices.Sort(methods)
		mux.HandleFunc(path, methodNotAllowed(methods))
	}
	mux.HandleFunc("/", notFound)
	return mux, nil
}

func methodNotAllowed(methods []string) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, ErrorCodeMethodNotAllowed, "this path does not accept that method")
	}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, ErrorCodeNotFound, "no such path")
}

func (s *server) handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"getHealth":            health,
		"createQuote":          s.createQuote,
		"createTrade":          s.createTrade,
		"getTradeByReference":  s.getTradeByReference,
		"createPayout":         s.createPayout,
		"getPayoutByReference": s.getPayoutByReference,
		"closeTrade":           s.closeTrade,
		"cancelPayout":         s.cancelPayout,
		"createFunding":        s.createFunding,
		"setRate":              s.setRate,
		"armScenario":          s.armScenario,
		"listScenarios":        s.listScenarios,
		"getRequestStats":      s.getRequestStats,
		"resetDataset":         s.resetDataset,
		"createStatement":      s.createStatement,
		"getStatementPage":     s.getStatementPage,
		"getStatementLine":     s.getStatementLine,
	}
}

func scheme(doc *openapi3.T, op *openapi3.Operation) string {
	requirements := doc.Security
	if op.Security != nil {
		requirements = *op.Security
	}
	for _, requirement := range requirements {
		for name := range requirement {
			return name
		}
	}
	return ""
}

type accountKey struct{}

type observationKey struct{}

type operationKey struct{}

func withOperation(id string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), operationKey{}, id)))
	}
}

func operationFrom(ctx context.Context) string {
	id, _ := ctx.Value(operationKey{}).(string)
	return id
}

func accountFrom(ctx context.Context) string {
	return ctx.Value(accountKey{}).(string)
}

// observe lets a handler name the reference and the faults it fired, for the request record.
func observe(ctx context.Context, reference string, faults ...string) {
	o, ok := ctx.Value(observationKey{}).(*provider.Observation)
	if !ok {
		return
	}
	o.Reference = reference
	if len(faults) > 0 {
		o.Fault = strings.Join(faults, ",")
	}
}

// dropReply simulates an answer lost after the commit: the connection closes with no response.
func dropReply(w http.ResponseWriter) {
	if recorder, ok := w.(*statusRecorder); ok {
		recorder.status = 0
	}
	panic(http.ErrAbortHandler)
}

func (s *server) provider(route *routers.Route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := s.accounts[sha256.Sum256([]byte(bearer(r)))]
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrorCodeUnauthorized, "a provider key is required")
			return
		}
		requestID := uuid.NewString()
		o := &provider.Observation{
			RequestID: requestID, Account: account, Route: route.Operation.OperationID,
			Reference: r.PathValue("reference"), Received: time.Now(),
		}
		ctx := context.WithValue(context.WithValue(r.Context(), accountKey{}, account), observationKey{}, o)
		ctx = provider.WithRequestID(ctx, requestID)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r.Body = http.MaxBytesReader(recorder, r.Body, maxBodyBytes)
		recorder.Header().Set("X-Request-Id", requestID)
		defer func() {
			o.Status = recorder.status
			if err := s.cfg.Service.Observe(context.WithoutCancel(ctx), *o); err != nil {
				slog.ErrorContext(ctx, "record request", "error", err)
			}
		}()

		admission, err := s.cfg.Service.Admit(ctx, account)
		if err != nil {
			internalError(recorder, r, err)
			return
		}
		o.Fault = admission.Fault
		if !admission.Admitted {
			o.Throttled = true
			recorder.Header().Set("Retry-After", strconv.Itoa(int(admission.RetryAfter/time.Second)))
			writeError(recorder, http.StatusTooManyRequests, ErrorCodeRateLimited, "over the request budget; nothing was executed")
			return
		}
		r = r.WithContext(ctx)
		if err := validate(r, route); err != nil {
			writeRequestError(recorder, err)
			return
		}
		next(recorder, r)
	}
}

func (s *server) admin(route *routers.Route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		supplied := sha256.Sum256([]byte(bearer(r)))
		if s.cfg.AdminKey == "" || subtle.ConstantTimeCompare(supplied[:], s.adminKey[:]) != 1 {
			writeError(w, http.StatusUnauthorized, ErrorCodeUnauthorized, "the admin key is required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := validate(r, route); err != nil {
			writeRequestError(w, err)
			return
		}
		next(w, r)
	}
}

func validate(r *http.Request, route *routers.Route) error {
	params := make(map[string]string)
	for _, match := range pathParam.FindAllStringSubmatch(route.Path, -1) {
		params[match[1]] = r.PathValue(match[1])
	}
	return openapi3filter.ValidateRequest(r.Context(), &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: params,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	})
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{Status: HealthStatusOk})
}

// decodeJSON reads the request body as T, answering the contract's error itself when it cannot.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRequestError(w, err)
		return body, false
	}
	return body, true
}

// writeRequestError answers a body the contract refuses: too large, or not what it describes.
func writeRequestError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, ErrorCodePayloadTooLarge, fmt.Sprintf("the body exceeds %d bytes", tooLarge.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	writeJSON(w, status, Error{Code: code, Message: message})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed",
		"operation", operationFrom(r.Context()),
		"route", r.Pattern,
		"error", err,
	)
	writeError(w, http.StatusInternalServerError, ErrorCodeInternal, "internal error")
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *provider.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, validation.Message)
	case errors.Is(err, provider.ErrConflict):
		writeError(w, http.StatusConflict, ErrorCodeReferenceConflict, provider.ErrConflict.Error())
	case errors.Is(err, provider.ErrQuoteUsed):
		writeError(w, http.StatusConflict, ErrorCodeQuoteAlreadyUsed, provider.ErrQuoteUsed.Error())
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorCodeNotFound, provider.ErrNotFound.Error())
	default:
		internalError(w, r, err)
	}
}

func parseAmount(s string) (int64, error) {
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, &provider.ValidationError{Message: fmt.Sprintf("amount %q does not fit in 64 bits", s)}
	}
	return value, nil
}

func formatAmount(v int64) string {
	return strconv.FormatInt(v, 10)
}

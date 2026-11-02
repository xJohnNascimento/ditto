package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
)

func (s *server) getRequestStats(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, "since must be an RFC 3339 time")
			return
		}
		since = parsed
	}
	stats, err := s.cfg.Service.RequestStats(r.Context(), since)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	body := RequestStats{Since: stats.Since.UTC(), PeakPerSecond: stats.PeakPerSecond, Routes: make([]RouteCount, len(stats.Routes))}
	for i, c := range stats.Routes {
		body.Routes[i] = RouteCount{Route: c.Route, Count: c.Count, Throttled: c.Throttled}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) resetDataset(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[ResetRequest](w, r)
	if !ok {
		return
	}
	if !slices.Contains([]string{"local", "ci"}, s.cfg.Profile) || s.cfg.DatasetID == "" || body.DatasetId != s.cfg.DatasetID {
		writeError(w, http.StatusForbidden, ErrorCodeForbidden, "reset needs the local or CI profile and this dataset's id")
		return
	}
	if err := s.cfg.Service.Reset(r.Context()); err != nil {
		writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) createStatement(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[CreateStatementRequest](w, r)
	if !ok {
		return
	}
	var after *uuid.UUID
	if body.AfterStatementId != nil {
		id, err := uuid.Parse(*body.AfterStatementId)
		if err != nil {
			writeError(w, http.StatusNotFound, ErrorCodeNotFound, "statement not found")
			return
		}
		after = &id
	}
	st, err := s.cfg.Service.CaptureStatement(r.Context(), accountFrom(r.Context()), after)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	currencies := make([]StatementCurrency, len(st.Currencies))
	for i, c := range st.Currencies {
		currencies[i] = StatementCurrency{
			Currency: c.Currency, OpeningBalance: c.OpeningBalance, ClosingBalance: c.ClosingBalance,
			FromSequence: c.FromSequence, ToSequence: c.ToSequence,
		}
	}
	writeJSON(w, http.StatusCreated, Statement{
		StatementId: st.ID.String(), CapturedAt: st.CapturedAt.UTC(), ContentHash: st.ContentHash,
		LineCount: st.LineCount, Currencies: currencies,
	})
}

func (s *server) getStatementPage(w http.ResponseWriter, r *http.Request) {
	id, ok := statementID(w, r)
	if !ok {
		return
	}
	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, "cursor is not one this API returned")
			return
		}
		cursor = parsed
	}
	lines, next, err := s.cfg.Service.StatementPage(r.Context(), accountFrom(r.Context()), id, cursor)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	page := struct {
		Lines      []json.RawMessage `json:"lines"`
		NextCursor string            `json:"next_cursor,omitempty"`
	}{Lines: lines}
	if page.Lines == nil {
		page.Lines = []json.RawMessage{}
	}
	if next > 0 {
		page.NextCursor = strconv.Itoa(next)
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *server) getStatementLine(w http.ResponseWriter, r *http.Request) {
	id, ok := statementID(w, r)
	if !ok {
		return
	}
	lineNo, err := strconv.Atoi(r.PathValue("lineId"))
	if err != nil || lineNo < 1 {
		writeError(w, http.StatusNotFound, ErrorCodeNotFound, "line not found")
		return
	}
	line, err := s.cfg.Service.StatementLine(r.Context(), accountFrom(r.Context()), id, lineNo)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, line)
}

func statementID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("statementId"))
	if err != nil {
		writeError(w, http.StatusNotFound, ErrorCodeNotFound, "statement not found")
		return uuid.Nil, false
	}
	return id, true
}

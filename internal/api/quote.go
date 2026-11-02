package api

import (
	"net/http"

	"github.com/xJohnNascimento/ditto/internal/provider"
)

func (s *server) createQuote(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[CreateQuoteRequest](w, r)
	if !ok {
		return
	}
	observe(r.Context(), body.ClientReference)
	sell, err := parseAmount(body.SellAmount)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	q, created, err := s.cfg.Service.CreateQuote(r.Context(), accountFrom(r.Context()), provider.QuoteRequest{
		ClientReference: body.ClientReference,
		SellCurrency:    body.SellCurrency,
		BuyCurrency:     body.BuyCurrency,
		SellAmount:      sell,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, createdOr(created), Quote{
		QuoteId:         q.ID.String(),
		ClientReference: q.ClientReference,
		SellCurrency:    q.SellCurrency,
		BuyCurrency:     q.BuyCurrency,
		SellAmount:      formatAmount(q.SellAmount),
		RateE8:          formatAmount(q.RateE8),
		BuyAmount:       formatAmount(q.BuyAmount),
		ExpiresAt:       q.ExpiresAt.UTC(),
	})
}

func (s *server) setRate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[Rate](w, r)
	if !ok {
		return
	}
	rate, err := parseAmount(body.RateE8)
	if err == nil {
		err = s.cfg.Service.SetRate(r.Context(), body.SellCurrency, body.BuyCurrency, rate)
	}
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

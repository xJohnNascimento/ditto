package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/ditto/internal/provider"
)

func (s *server) createTrade(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[CreateTradeRequest](w, r)
	if !ok {
		return
	}
	observe(r.Context(), body.Reference)
	quoteID, err := uuid.Parse(body.QuoteId)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, "quote_id is not a quote id")
		return
	}
	sell, err := parseAmount(body.SellAmount)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	t, created, err := s.cfg.Service.CreateTrade(r.Context(), accountFrom(r.Context()), provider.TradeRequest{
		Reference: body.Reference, QuoteID: quoteID, SellCurrency: body.SellCurrency, BuyCurrency: body.BuyCurrency, SellAmount: sell,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	observe(r.Context(), body.Reference, t.Faults...)
	if t.DropReply {
		dropReply(w)
	}
	writeJSON(w, createdOr(created), tradeBody(t))
}

func (s *server) getTradeByReference(w http.ResponseWriter, r *http.Request) {
	t, err := s.cfg.Service.Trade(r.Context(), accountFrom(r.Context()), r.PathValue("reference"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tradeBody(t))
}

func (s *server) createPayout(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[CreatePayoutRequest](w, r)
	if !ok {
		return
	}
	observe(r.Context(), body.Reference)
	amount, err := parseAmount(body.Amount)
	if err != nil || amount == 0 {
		writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, "amount must be a positive 64-bit integer")
		return
	}
	remittance := ""
	if body.RemittanceInfo != nil {
		remittance = *body.RemittanceInfo
	}
	p, created, err := s.cfg.Service.CreatePayout(r.Context(), accountFrom(r.Context()), provider.PayoutRequest{
		Reference: body.Reference,
		Currency:  body.Currency,
		Amount:    amount,
		Beneficiary: provider.Beneficiary{
			ID: body.Beneficiary.Id, Name: body.Beneficiary.Name, Account: body.Beneficiary.Account,
			BankCode: body.Beneficiary.BankCode, Country: body.Beneficiary.Country,
		},
		RemittanceInfo: remittance,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	observe(r.Context(), body.Reference, p.Faults...)
	if p.DropReply {
		dropReply(w)
	}
	writeJSON(w, createdOr(created), payoutBody(p))
}

func (s *server) getPayoutByReference(w http.ResponseWriter, r *http.Request) {
	p, err := s.cfg.Service.Payout(r.Context(), accountFrom(r.Context()), r.PathValue("reference"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, payoutBody(p))
}

func (s *server) createFunding(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[FundingRequest](w, r)
	if !ok {
		return
	}
	amount, err := parseAmount(body.Amount)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	m, created, err := s.cfg.Service.Fund(r.Context(), provider.FundingRequest{
		Account: body.Account, Reference: body.Reference, Currency: body.Currency, Amount: amount,
	})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, createdOr(created), movementBody(m))
}

func createdOr(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

func tradeBody(t provider.Trade) Trade {
	body := Trade{Reference: t.Reference, Status: TradeStatus(t.Status)}
	if !t.HasTerms {
		return body
	}
	id, sell, buy, sold, bought, rate := t.ID.String(), t.SellCurrency, t.BuyCurrency, formatAmount(t.SellAmount), formatAmount(t.BuyAmount), formatAmount(t.RateE8)
	executed := t.ExecutedAt.UTC()
	body.TradeId, body.SellCurrency, body.BuyCurrency, body.SellAmount, body.BuyAmount, body.RateE8, body.ExecutedAt = &id, &sell, &buy, &sold, &bought, &rate, &executed
	if t.Reason != "" {
		reason := TradeReason(t.Reason)
		body.Reason = &reason
	}
	return body
}

func payoutBody(p provider.Payout) Payout {
	body := Payout{Reference: p.Reference, Status: PayoutStatus(p.Status), Sequence: p.Sequence}
	if p.FailureCode != "" {
		code := p.FailureCode
		body.FailureCode = &code
	}
	if !p.HasTerms {
		return body
	}
	id, currency, amount := p.ID.String(), p.Currency, formatAmount(p.Amount)
	body.PayoutId, body.Currency, body.Amount = &id, &currency, &amount
	return body
}

func movementBody(m provider.Movement) Movement {
	return Movement{
		Sequence: m.Sequence, Currency: m.Currency, Amount: formatAmount(m.Amount), BalanceAfter: formatAmount(m.BalanceAfter),
		Kind: MovementKind(m.Kind), Reference: m.Reference,
	}
}

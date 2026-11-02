package api

import (
	"net/http"
	"strconv"

	"github.com/xJohnNascimento/ditto/internal/provider"
)

func (s *server) closeTrade(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	state, err := s.cfg.Service.CloseTrade(r.Context(), accountFrom(r.Context()), reference)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, FenceResult{Reference: reference, Status: FenceResultStatus(state)})
}

func (s *server) cancelPayout(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	state, err := s.cfg.Service.CancelPayout(r.Context(), accountFrom(r.Context()), reference)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, FenceResult{Reference: reference, Status: FenceResultStatus(state)})
}

func (s *server) armScenario(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[ArmScenarioRequest](w, r)
	if !ok {
		return
	}
	var args provider.FaultArgs
	if a := body.Args; a != nil {
		if a.Delta != nil {
			delta, err := strconv.ParseInt(*a.Delta, 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, ErrorCodeValidationFailed, "delta does not fit in 64 bits")
				return
			}
			args.Delta = delta
		}
		args.Code = deref(a.Code)
		args.DelayMS = int64(deref(a.DelayMs))
		args.DurationMS = int64(deref(a.DurationMs))
	}
	uses := 1
	if body.Uses != nil {
		uses = *body.Uses
	}
	arm, err := s.cfg.Service.ArmFault(r.Context(), string(body.Fault), string(body.Target.Kind), body.Target.Value, args, uses)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, armBody(arm))
}

func (s *server) listScenarios(w http.ResponseWriter, r *http.Request) {
	arms, err := s.cfg.Service.Arms(r.Context())
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	list := ScenarioArmList{Arms: make([]ScenarioArm, len(arms))}
	for i, a := range arms {
		list.Arms[i] = armBody(a)
	}
	writeJSON(w, http.StatusOK, list)
}

func armBody(a provider.Arm) ScenarioArm {
	args := &ScenarioArgs{}
	if a.Args.Delta != 0 {
		delta := formatAmount(a.Args.Delta)
		args.Delta = &delta
	}
	if a.Args.Code != "" {
		args.Code = &a.Args.Code
	}
	if a.Args.DelayMS != 0 {
		delay := int(a.Args.DelayMS)
		args.DelayMs = &delay
	}
	if a.Args.DurationMS != 0 {
		duration := int(a.Args.DurationMS)
		args.DurationMs = &duration
	}
	body := ScenarioArm{
		Id: a.ID.String(), Fault: Fault(a.Fault), Target: ScenarioTarget{Kind: ScenarioTargetKind(a.TargetKind), Value: a.TargetValue},
		Args: args, RemainingUses: a.RemainingUses, Consumptions: make([]ScenarioConsumption, len(a.Consumptions)),
	}
	for i, c := range a.Consumptions {
		body.Consumptions[i] = ScenarioConsumption{RequestId: c.RequestID, ConsumedAt: c.ConsumedAt.UTC()}
	}
	return body
}

func deref[T any](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}

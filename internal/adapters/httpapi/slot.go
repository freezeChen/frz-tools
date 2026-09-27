package httpapi

import (
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// handleListSlots 返回两个槽位的运营视图与线上实际（迭代 4c）。
//
// 它**不写库**：只读命令有副作用是坏味道。库与线上不一致时用 `inconsistent` 标出来
// ——那件事本身就是要看的信息，由启动对账去纠正，理由见 SlotService.List。
func (s *Server) handleListSlots(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "slot service is not configured"))
		return
	}
	view, err := s.deps.Slots.List(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, slotListDTO(view))
}

// handleSlotHistory 返回切换时间线。
func (s *Server) handleSlotHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "slot service is not configured"))
		return
	}
	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	history, err := s.deps.Slots.History(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	response := v1.SlotHistoryResponse{
		APIVersion:  v1.APIVersion,
		Application: history.Application.ID,
		Items:       make([]v1.SlotEvent, 0, len(history.Events)),
	}
	for _, event := range history.Events {
		response.Items = append(response.Items, v1.SlotEvent{
			ID: event.ID, Slot: string(event.Slot), ReleaseID: event.ReleaseID,
			Version: event.Version, Kind: string(event.Kind), Detail: event.Detail,
			OperationID: event.OperationID, At: event.At,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func slotListDTO(view *application.SlotView) v1.SlotListResponse {
	response := v1.SlotListResponse{
		APIVersion:   v1.APIVersion,
		Application:  view.Application.ID,
		ServingSlot:  string(view.ServingSlot),
		RecordedSlot: string(view.RecordedSlot),
		OnlineKnown:  view.OnlineKnown,
		Inconsistent: view.Inconsistent,
		Detail:       view.Detail,
		Items:        make([]v1.SlotStatus, 0, len(view.Slots)),
	}
	for _, slot := range view.Slots {
		item := v1.SlotStatus{
			Slot:         string(slot.Slot),
			State:        string(slot.State),
			ReleaseID:    slot.ReleaseID,
			Version:      slot.Version,
			Ports:        slot.Ports,
			UnitName:     slot.UnitName,
			Serving:      slot.Serving,
			ProcessState: string(slot.ProcessState),
			ProcessKnown: slot.ProcessKnown,
			ReadyDetail:  slot.ReadyDetail,
			SwitchedAt:   slot.SwitchedAt,
			Detail:       slot.Detail,
		}
		if slot.ReadyProbed {
			ready := slot.Ready
			item.Ready = &ready
		}
		if !slot.UpdatedAt.IsZero() {
			updated := slot.UpdatedAt
			item.UpdatedAt = &updated
		}
		response.Items = append(response.Items, item)
	}
	return response
}

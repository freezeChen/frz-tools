package httpapi

import (
	"net/http"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	var req v1.CreateScheduleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	if req.IntervalSeconds < 0 {
		writeError(w, s.logger(), domain.NewError(v1.CodeScheduleInvalid, "intervalSeconds 不能为负数"))
		return
	}

	schedule, err := s.deps.Schedules.Create(r.Context(), application.CreateScheduleInput{
		Name:            req.Name,
		Kind:            domain.ScheduleKind(req.Kind),
		Cron:            req.Cron,
		Interval:        time.Duration(req.IntervalSeconds) * time.Second,
		Timezone:        req.Timezone,
		Resource:        req.Resource,
		Spec:            req.Spec,
		MissedRunPolicy: domain.MissedRunPolicy(req.MissedRunPolicy),
		CreatedBy:       req.CreatedBy,
	})
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusCreated, v1.ScheduleResponse{
		APIVersion: v1.APIVersion,
		Schedule:   scheduleDTO(schedule),
	})
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	schedules, err := s.deps.Schedules.List(r.Context(), limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.Schedule, 0, len(schedules))
	for i := range schedules {
		items = append(items, scheduleDTO(&schedules[i]))
	}
	writeJSON(w, http.StatusOK, v1.ScheduleListResponse{APIVersion: v1.APIVersion, Items: items})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	schedule, err := s.deps.Schedules.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ScheduleResponse{
		APIVersion: v1.APIVersion,
		Schedule:   scheduleDTO(schedule),
	})
}

// 启用与停用刻意用两条显式路由，而不是 /{action} 通配：通配会把
// /schedules/{id}/runs 这类路径也吞掉，并把未知动作当成停用处理。
func (s *Server) handleEnableSchedule(w http.ResponseWriter, r *http.Request) {
	s.setScheduleEnabled(w, r, true)
}

func (s *Server) handleDisableSchedule(w http.ResponseWriter, r *http.Request) {
	s.setScheduleEnabled(w, r, false)
}

func (s *Server) setScheduleEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	schedule, err := s.deps.Schedules.SetEnabled(r.Context(), r.PathValue("id"), enabled)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ScheduleResponse{
		APIVersion: v1.APIVersion,
		Schedule:   scheduleDTO(schedule),
	})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	if err := s.deps.Schedules.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScheduleRuns(w http.ResponseWriter, r *http.Request) {
	if s.deps.Schedules == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "调度服务未装配"))
		return
	}

	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	schedule, err := s.deps.Schedules.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	runs, err := s.deps.Schedules.Runs(r.Context(), schedule.ID, limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.ScheduleRun, 0, len(runs))
	for i := range runs {
		items = append(items, scheduleRunDTO(&runs[i]))
	}
	writeJSON(w, http.StatusOK, v1.ScheduleRunListResponse{
		APIVersion: v1.APIVersion,
		Schedule:   schedule.ID,
		Items:      items,
	})
}

func scheduleDTO(schedule *domain.Schedule) v1.Schedule {
	return v1.Schedule{
		ID:              schedule.ID,
		Name:            schedule.Name,
		Enabled:         schedule.Enabled,
		Kind:            string(schedule.Kind),
		Cron:            schedule.Cron,
		IntervalSeconds: int(schedule.Interval / time.Second),
		Timezone:        schedule.Timezone,
		Resource:        schedule.Resource,
		Spec:            schedule.Spec,
		MissedRunPolicy: string(schedule.MissedRunPolicy),
		NextRunAt:       schedule.NextRunAt,
		LastRunAt:       schedule.LastRunAt,
		LastResult:      schedule.LastResult,
		CreatedAt:       schedule.CreatedAt,
		UpdatedAt:       schedule.UpdatedAt,
		CreatedBy:       schedule.CreatedBy,
	}
}

func scheduleRunDTO(run *domain.ScheduleRun) v1.ScheduleRun {
	return v1.ScheduleRun{
		ID:           run.ID,
		ScheduleID:   run.ScheduleID,
		ScheduledFor: run.ScheduledFor,
		StartedAt:    run.StartedAt,
		FinishedAt:   run.FinishedAt,
		Result:       string(run.Result),
		OperationID:  run.OperationID,
		ErrorCode:    run.ErrorCode,
		ErrorMessage: run.ErrorMessage,
	}
}

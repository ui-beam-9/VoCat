package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"vocat/internal/store"
)

func TestNextAutomaticRunUsesIntervalAndLocalClock(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, location)
	next, err := nextAutomaticRun("2026-08-01", "09:30", 3, now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 13, 9, 30, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("next run = %v, want %v", next, want)
	}
}

// 验证任务正文仅移除重复标题，保留普通首行且不修改原始文本。
func TestAutomaticTaskNotificationDetailTextRemovesTitle(t *testing.T) {
	message := automaticTaskNotification{
		Title: "自动任务执行成功",
		Text:  "自动任务执行成功\n任务  检查网络\n结果  任务已完成",
	}
	if got := message.DetailText(); got != "任务  检查网络\n结果  任务已完成" {
		t.Fatalf("DetailText() = %q", got)
	}
	if message.Text != "自动任务执行成功\n任务  检查网络\n结果  任务已完成" {
		t.Fatalf("DetailText() changed the original text to %q", message.Text)
	}

	message.Text = "正文第一行\n正文第二行"
	if got := message.DetailText(); got != message.Text {
		t.Fatalf("DetailText() changed non-title body to %q", got)
	}

	message.Text = "提醒：自动任务执行成功后请确认\n结果  任务已完成"
	if got := message.DetailText(); got != message.Text {
		t.Fatalf("DetailText() changed a first line merely mentioning the title to %q", got)
	}
}

func TestUSBSIMReaderAutomaticTasksRequireVoWiFi(t *testing.T) {
	reader := store.Device{DeviceType: store.DeviceTypeUSBSIMReader}
	for _, test := range []struct {
		taskType    string
		environment string
		wantError   bool
	}{
		{taskType: "sms", environment: "vowifi"},
		{taskType: "call", environment: "vowifi"},
		{taskType: "sms", environment: "cellular", wantError: true},
		{taskType: "public_ip", environment: "cellular", wantError: true},
	} {
		err := validateAutomaticTaskDeviceCapabilities(reader, test.taskType, test.environment)
		if (err != nil) != test.wantError {
			t.Errorf("type=%s environment=%s error=%v", test.taskType, test.environment, err)
		}
	}
}

func TestAutomaticTaskAvailabilityHidesRestrictedPaths(t *testing.T) {
	for _, test := range []struct {
		available   bool
		taskType    string
		environment string
		wantError   bool
	}{
		{false, "sms", "vowifi", false},
		{false, "call", "vowifi", false},
		{false, "sms", "cellular", false},
		{false, "call", "cellular", false},
		{false, "public_ip", "cellular", true},
		{true, "public_ip", "cellular", false},
	} {
		err := validateAutomaticTaskAvailability(test.available, test.taskType, test.environment)
		if (err != nil) != test.wantError {
			t.Fatalf("availability(%v, %q, %q) = %v", test.available, test.taskType, test.environment, err)
		}
	}
}

func TestAutomaticSMSRetrySafetyPreventsDuplicateSubmission(t *testing.T) {
	unsafe := []byte(`{"data":{"parts_attempted":1,"parts_accepted":1,"retry_safe":false}}`)
	if automaticSMSRetrySafe(unsafe) {
		t.Fatal("partially submitted SMS was considered safe to retry")
	}
	safe := []byte(`{"data":{"parts_attempted":0,"parts_accepted":0}}`)
	if !automaticSMSRetrySafe(safe) {
		t.Fatal("unattempted SMS was not considered safe to retry")
	}
}

// 执行记录删除接口：排队中/执行中返回 409，已结束返回 200，不存在返回 404，
// 非法 ID 返回 400。
func TestDeleteAutomaticTaskRunEndpoint(t *testing.T) {
	server := blockedRegionServer(t, "310260123456789")
	ctx := context.Background()
	if err := server.store.UpsertDevice(ctx, store.Device{ID: "dev1", Name: "Modem"}); err != nil {
		t.Fatal(err)
	}
	task, err := server.store.SaveAutomaticTask(ctx, store.AutomaticTask{
		Name: "task", Enabled: true, DeviceID: "dev1", ProfileICCID: "test-card",
		TaskType: "sms", Environment: "vowifi", IntervalDays: 1,
		StartDate: "2026-10-31", RunTime: "12:00", Timezone: "Asia/Shanghai",
		Payload: []byte(`{}`), NextRunAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := server.store.QueueAutomaticTaskNow(ctx, task)
	if err != nil {
		t.Fatal(err)
	}

	if status, body := deleteAutomaticTaskRunRequest(server, run.ID); status != http.StatusConflict {
		t.Fatalf("queued run delete status = %d (%s), want 409", status, body)
	}

	run.StartedAt, run.FinishedAt, run.Status = time.Now().UTC(), time.Now().UTC(), "success"
	if err := server.store.UpdateAutomaticTaskRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if status, body := deleteAutomaticTaskRunRequest(server, run.ID); status != http.StatusOK {
		t.Fatalf("finished run delete status = %d (%s), want 200", status, body)
	}
	if status, body := deleteAutomaticTaskRunRequest(server, run.ID); status != http.StatusNotFound {
		t.Fatalf("missing run delete status = %d (%s), want 404", status, body)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/automatic-tasks/runs/abc", nil)
	if !server.routeAutomaticTasksAPI(recorder, request, "automatic-tasks/runs/abc") {
		t.Fatal("invalid run ID route was not handled")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid run ID status = %d, want 400", recorder.Code)
	}

	runs, err := server.store.ListAutomaticTaskRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs after delete = %+v, want none", runs)
	}
}

func deleteAutomaticTaskRunRequest(server *Server, id int64) (int, string) {
	path := "automatic-tasks/runs/" + strconv.FormatInt(id, 10)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/"+path, nil)
	if !server.routeAutomaticTasksAPI(recorder, request, path) {
		return 0, "route not handled"
	}
	return recorder.Code, recorder.Body.String()
}

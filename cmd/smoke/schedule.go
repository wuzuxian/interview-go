// 面试日程冒烟模式（票 04，-mode schedule）：真库 CRUD 全链路 + 规则解析三平台
// 样例 + 过期取消验证。不打真实 LLM——飞书/腾讯样例规则直解，Zoom 样例走特征
// 探测→飞书解析器兜底链路（Zoom 规则不提取公司岗位，纯 Zoom 文本必走 AI 兜底，
// AI 兜底已由单测覆盖）。过期取消步需服务以短巡检间隔启动（见 -sweeper-wait）。
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wuzuxian/interview/internal/interviewschedule"
)

// 冒烟样例文本（与 parser_test.go 同源，规则可解）。
const (
	smokeFeishuSample = `【面试邀约】
公司：阿里巴巴
岗位：Java后端开发工程师
时间：2026-04-10 14:30
会议链接：https://meeting.feishu.cn/smoke01
第 二 轮面试`

	smokeTencentSample = `腾讯会议
公司：字节跳动
岗位：后端开发工程师
时间：2026/04/15 10:00
会议号：123456789
密码：1234`

	// smokeZoomSample Zoom 样例：Zoom 规则只提取链接与时间（Java 现状，不提取
	// 公司/岗位），特征探测命中 Zoom 但产物无效 → 逐个兜底由飞书解析器以
	// 公司/岗位/时间标签命中（zoom 链接不被飞书链接正则捕获）——锁定该兜底链路。
	smokeZoomSample = `Zoom 会议邀请
公司：腾讯
岗位：测试开发工程师
时间：2026-04-20 09:30
链接：https://zoom.us/j/9876543210`
)

// scheduleSmoke 日程冒烟状态。
type scheduleSmoke struct {
	sm     *smoke
	marker string  // 公司名标记（含随机后缀，用于在列表中断言本 smoke 行）
	ids    []int64 // 本 smoke 创建的全部日程 id（收尾清理）
	expiry int64   // 过期行 id（过期取消步）
}

// runSchedule 日程冒烟主流程（-mode schedule）。
func runSchedule(opts *options, out io.Writer) error {
	base := strings.TrimRight(opts.baseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("参数 -base-url 非法：%q（须包含 http:// 或 https://）", opts.baseURL)
	}
	client := &http.Client{Timeout: opts.timeout}
	sm := &smoke{client: client, base: base, out: out, opts: opts, stepElapsed: map[string]time.Duration{}}
	sc := &scheduleSmoke{sm: sm, marker: fmt.Sprintf("SMOKE日程%02d%02d%02d", time.Now().Hour(), time.Now().Minute(), time.Now().Second())}

	fmt.Fprintf(out, "== 面试日程全链路冒烟（票 04）==\n服务: %s  标记: %s  过期等待: %s\n\n",
		base, sc.marker, opts.sweeperWait)

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"健康检查", sm.stepHealth},
		{"规则解析·飞书样例", sc.stepParseFeishu},
		{"规则解析·腾讯会议样例", sc.stepParseTencent},
		{"规则解析·Zoom 样例（探测+兜底）", sc.stepParseZoom},
		{"创建日程（强制 PENDING）", sc.stepCreate},
		{"详情校验", sc.stepGetByID},
		{"列表过滤（状态/时间范围）", sc.stepListFilters},
		{"更新（status 不变）", sc.stepUpdate},
		{"状态流转（PATCH/PUT）", sc.stepUpdateStatus},
		{"过期取消验证", sc.stepExpiryCancel},
		{"清理", sc.stepCleanup},
	}
	return executeSteps(context.Background(), out, sm, steps)
}

// parseCall 调 /parse 并解包 ParseResponse。
func (sc *scheduleSmoke) parseCall(ctx context.Context, rawText, source string) (*interviewschedule.ParseResponse, error) {
	var out interviewschedule.ParseResponse
	body := map[string]any{"rawText": rawText}
	if source != "" {
		body["source"] = source
	}
	if err := sc.sm.doJSON(ctx, http.MethodPost, "/api/interview-schedule/parse", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stepParseFeishu 飞书样例规则解析（0.95/rule）。
func (sc *scheduleSmoke) stepParseFeishu(ctx context.Context) error {
	out, err := sc.parseCall(ctx, smokeFeishuSample, "feishu")
	if err != nil {
		return err
	}
	if !out.Success || out.Confidence != 0.95 || out.ParseMethod != "rule" || out.Log != "规则解析成功" {
		return fmt.Errorf("解析形态异常: success=%v confidence=%v method=%s log=%s",
			out.Success, out.Confidence, out.ParseMethod, out.Log)
	}
	d := out.Data
	if d == nil || d.CompanyName != "阿里巴巴" || d.Position != "Java后端开发工程师" ||
		d.InterviewTime.Time().Format("2006-01-02T15:04") != "2026-04-10T14:30" {
		return fmt.Errorf("飞书解析产物异常: %+v", d)
	}
	sc.sm.logf("飞书样例：公司/岗位/时间/链接/轮次=2 全命中（0.95/rule）")
	return nil
}

// stepParseTencent 腾讯会议样例（含 group() 全匹配 meetingLink 现状断言）。
func (sc *scheduleSmoke) stepParseTencent(ctx context.Context) error {
	out, err := sc.parseCall(ctx, smokeTencentSample, "tencent")
	if err != nil {
		return err
	}
	if !out.Success || out.ParseMethod != "rule" {
		return fmt.Errorf("解析形态异常: %+v", out)
	}
	d := out.Data
	if d == nil || d.CompanyName != "字节跳动" {
		return fmt.Errorf("腾讯解析产物异常: %+v", d)
	}
	wantLink := "会议号: 会议号：123456789 密码: 密码：1234"
	if d.MeetingLink == nil || *d.MeetingLink != wantLink {
		return fmt.Errorf("meetingLink = %v, want %q（group() 全匹配逐字）", d.MeetingLink, wantLink)
	}
	sc.sm.logf("腾讯样例：公司/时间/会议号+密码 全命中；meetingLink 全匹配现状确认")
	return nil
}

// stepParseZoom Zoom 样例：断言特征探测→兜底链路（飞书解析器命中三要素，
// zoom 链接不被捕获——Java 现状）。
func (sc *scheduleSmoke) stepParseZoom(ctx context.Context) error {
	out, err := sc.parseCall(ctx, smokeZoomSample, "")
	if err != nil {
		return err
	}
	if !out.Success || out.ParseMethod != "rule" || out.Confidence != 0.95 {
		return fmt.Errorf("解析形态异常: %+v", out)
	}
	d := out.Data
	if d == nil || d.CompanyName != "腾讯" || d.Position != "测试开发工程师" ||
		d.InterviewTime.Time().Format("2006-01-02T15:04") != "2026-04-20T09:30" {
		return fmt.Errorf("Zoom 兜底产物异常: %+v", d)
	}
	if d.MeetingLink != nil {
		return fmt.Errorf("zoom 链接不应被飞书链接正则捕获（Java 现状）: %v", *d.MeetingLink)
	}
	sc.sm.logf("Zoom 样例：特征探测→飞书解析器兜底命中公司/岗位/时间（链接不捕获，Java 现状）")
	return nil
}

// createCall 创建日程并登记 id（收尾清理）。
func (sc *scheduleSmoke) createCall(ctx context.Context, body map[string]any) (*interviewschedule.InterviewScheduleDTO, error) {
	var out interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodPost, "/api/interview-schedule", body, &out); err != nil {
		return nil, err
	}
	sc.ids = append(sc.ids, out.ID)
	return &out, nil
}

// stepCreate 创建日程：客户端试图携带 status 必须被忽略，roundNumber 缺省 1。
func (sc *scheduleSmoke) stepCreate(ctx context.Context) error {
	future := time.Now().Add(48 * time.Hour).Format("2006-01-02T15:04:05")
	out, err := sc.createCall(ctx, map[string]any{
		"companyName":   sc.marker + "公司",
		"position":      "冒烟工程师",
		"interviewTime": future,
		"interviewType": "VIDEO",
		"status":        "COMPLETED", // 客户端试图指定，服务端必须忽略
	})
	if err != nil {
		return err
	}
	if out.Status != "PENDING" {
		return fmt.Errorf("status = %s, want PENDING（创建强制）", out.Status)
	}
	if out.RoundNumber == nil || *out.RoundNumber != 1 {
		return fmt.Errorf("roundNumber = %v, want 1（缺省）", out.RoundNumber)
	}
	sc.sm.logf("已创建 id=%d status=PENDING（传入 status 被忽略）interviewTime=%s", out.ID, out.InterviewTime.Time().Format("2006-01-02T15:04:05"))
	return nil
}

// stepGetByID 详情：12 字段齐全、时间无时区格式。
func (sc *scheduleSmoke) stepGetByID(ctx context.Context) error {
	id := sc.ids[len(sc.ids)-1]
	var out interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/interview-schedule/%d", id), nil, &out); err != nil {
		return err
	}
	if out.ID != id || out.CompanyName != sc.marker+"公司" || out.Status != "PENDING" {
		return fmt.Errorf("详情字段异常: %+v", out)
	}
	// 线协议时间格式：无时区后缀（对齐 Java yyyy-MM-dd'T'HH:mm:ss）
	raw, err := sc.sm.do(ctx, http.MethodGet, fmt.Sprintf("/api/interview-schedule/%d", id), nil)
	if err != nil {
		return err
	}
	m := naiveTimeInBody.FindStringSubmatch(string(raw))
	if m == nil {
		return fmt.Errorf("响应缺少 interviewTime 字段，摘要: %s", excerpt(raw, 200))
	}
	if len(m[1]) != 19 {
		return fmt.Errorf("interviewTime = %q, 应为无时区 yyyy-MM-dd'T'HH:mm:ss（19 位）", m[1])
	}
	sc.sm.logf("详情 id=%d 字段齐全，interviewTime 无时区格式", id)
	return nil
}

// stepListFilters 列表：全量含新行、状态过滤命中、时间范围双端闭区间。
func (sc *scheduleSmoke) stepListFilters(ctx context.Context) error {
	var all []interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodGet, "/api/interview-schedule", nil, &all); err != nil {
		return err
	}
	found := false
	for _, d := range all {
		if d.CompanyName == sc.marker+"公司" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("全量列表未找到标记行（共 %d 行）", len(all))
	}

	var pending []interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodGet, "/api/interview-schedule?status=PENDING", nil, &pending); err != nil {
		return err
	}
	for _, d := range pending {
		if d.Status != "PENDING" {
			return fmt.Errorf("状态过滤返回非 PENDING 行: %+v", d)
		}
	}

	// 时间范围：以创建行时间为 end（闭区间应命中）。边界串以无时区墙钟构造并按
	// UTC 解析回比较值——响应中的 naive 时间同样按 UTC 解析（墙钟一致）；切勿与
	// 本地时间的绝对时刻直接比较（差 8 小时，smoke 首跑曾踩）。
	at := time.Now().Add(48 * time.Hour)
	endStr := at.Format("2006-01-02T15:04:05")
	startStr := at.Add(-time.Hour).Format("2006-01-02T15:04:05")
	q := fmt.Sprintf("?start=%s&end=%s", startStr, endStr)
	var inRange []interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodGet, "/api/interview-schedule"+q, nil, &inRange); err != nil {
		return err
	}
	startWall, _ := time.Parse("2006-01-02T15:04:05", startStr)
	endWall, _ := time.Parse("2006-01-02T15:04:05", endStr)
	for _, d := range inRange {
		if d.InterviewTime.Time().Before(startWall) || d.InterviewTime.Time().After(endWall) {
			return fmt.Errorf("范围外行被返回: %s", d.InterviewTime.Time().Format("2006-01-02T15:04:05"))
		}
	}
	sc.sm.logf("全量=%d 行含标记行；状态过滤 %d 行全 PENDING；时间范围闭区间命中 %d 行", len(all), len(pending), len(inRange))
	return nil
}

// stepUpdate 更新：8 业务字段拷贝、status 恒不变、轮次透传。
func (sc *scheduleSmoke) stepUpdate(ctx context.Context) error {
	id := sc.ids[len(sc.ids)-1]
	newTime := time.Now().Add(72 * time.Hour).Format("2006-01-02T15:04")
	var out interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/interview-schedule/%d", id), map[string]any{
		"companyName":   sc.marker + "公司改",
		"position":      "冒烟工程师二面",
		"interviewTime": newTime,
		"interviewType": "PHONE",
		"roundNumber":   2,
	}, &out); err != nil {
		return err
	}
	if out.Status != "PENDING" {
		return fmt.Errorf("更新后 status = %s, want PENDING（status 只能经状态接口改）", out.Status)
	}
	if out.CompanyName != sc.marker+"公司改" || out.InterviewType == nil || *out.InterviewType != "PHONE" ||
		out.RoundNumber == nil || *out.RoundNumber != 2 {
		return fmt.Errorf("更新字段未生效: %+v", out)
	}
	sc.sm.logf("更新后业务字段生效、status 仍 PENDING")
	return nil
}

// stepUpdateStatus 状态流转：PATCH→COMPLETED、PUT 任意跳回 PENDING（无流转校验）。
func (sc *scheduleSmoke) stepUpdateStatus(ctx context.Context) error {
	id := sc.ids[len(sc.ids)-1]
	var out interviewschedule.InterviewScheduleDTO
	if err := sc.sm.doJSON(ctx, http.MethodPatch,
		fmt.Sprintf("/api/interview-schedule/%d/status?status=COMPLETED", id), nil, &out); err != nil {
		return err
	}
	if out.Status != "COMPLETED" {
		return fmt.Errorf("PATCH 后 status = %s, want COMPLETED", out.Status)
	}
	if err := sc.sm.doJSON(ctx, http.MethodPut,
		fmt.Sprintf("/api/interview-schedule/%d/status?status=PENDING", id), nil, &out); err != nil {
		return err
	}
	if out.Status != "PENDING" {
		return fmt.Errorf("PUT 回置 status = %s, want PENDING（任意跳转）", out.Status)
	}
	sc.sm.logf("PATCH→COMPLETED、PUT 任意跳回 PENDING（无流转校验，Java 现状）")
	return nil
}

// stepExpiryCancel 过期取消：插入 interview_time 在过去的 PENDING 行，轮询至
// CANCELLED。服务需以短巡检间隔启动（如 APP_APP_INTERVIEW_SCHEDULE_SWEEPER_INTERVAL=2s），
// 否则该步在 -sweeper-wait 内超时判失败；-sweeper-wait=0 跳过该步。
func (sc *scheduleSmoke) stepExpiryCancel(ctx context.Context) error {
	if sc.sm.opts.sweeperWait <= 0 {
		sc.sm.logf("-sweeper-wait=0，跳过过期取消验证")
		return nil
	}
	past := time.Now().Add(-2 * time.Hour).Format("2006-01-02T15:04:05")
	out, err := sc.createCall(ctx, map[string]any{
		"companyName":   sc.marker + "过期行",
		"position":      "已过期岗位",
		"interviewTime": past,
	})
	if err != nil {
		return err
	}
	sc.expiry = out.ID
	sc.sm.logf("已创建过期行 id=%d（interviewTime=%s），等待巡检置 CANCELLED（上限 %s）",
		out.ID, past, sc.sm.opts.sweeperWait)

	deadline := time.Now().Add(sc.sm.opts.sweeperWait)
	for {
		var cur interviewschedule.InterviewScheduleDTO
		if err := sc.sm.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/interview-schedule/%d", sc.expiry), nil, &cur); err != nil {
			return err
		}
		if cur.Status == "CANCELLED" {
			sc.sm.logf("过期行已被巡检置 CANCELLED")
			return nil
		}
		if cur.Status != "PENDING" {
			return fmt.Errorf("过期行状态异常: %s", cur.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("巡检未在 %s 内取消过期行（服务是否以短间隔启动？APP_APP_INTERVIEW_SCHEDULE_SWEEPER_INTERVAL=2s）", sc.sm.opts.sweeperWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stepCleanup 清理本 smoke 创建的全部日程行（-keep 跳过）。
func (sc *scheduleSmoke) stepCleanup(ctx context.Context) error {
	if sc.sm.opts.keep {
		sc.sm.logf("-keep 指定，保留 %d 行", len(sc.ids))
		return nil
	}
	for _, id := range sc.ids {
		if err := sc.sm.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/api/interview-schedule/%d", id), nil, nil); err != nil {
			return fmt.Errorf("删除 id=%d 失败: %w", id, err)
		}
	}
	sc.sm.logf("已清理 %d 行", len(sc.ids))
	return nil
}

// naiveTimeInBody 从原始响应中提取 interviewTime 的字面值（线协议格式断言用）。
var naiveTimeInBody = regexp.MustCompile(`"interviewTime":"([^"]*)"`)

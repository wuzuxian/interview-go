// 分类题量分配：从 Java InterviewSkillService.calculateAllocation /
// buildAllocationDescription（笔记 04 §5）逐字移植。
package interview

import "strconv"

// CalculateAllocation 三阶段优先级分配（对齐 Java calculateAllocation，252-315 行）：
// Phase 1 ALWAYS_ONE 各保底 1 题 → Phase 2 全类目各 1 题（CORE 先于 NORMAL，保证覆盖率）
// → Phase 3 剩余名额按 CORE 优先、CORE/NORMAL 轮转。
// priority 非 ALWAYS_ONE/CORE 一律按 NORMAL（Java switch default）。
// 结束后 CORE/NORMAL 全类目都出现在结果中（数量可为 0）；ALWAYS_ONE 未获配时不出现。
// 调用侧经 categories 顺序遍历该结果，故 map 无序不影响输出稳定性。
func CalculateAllocation(categories []CategoryDTO, totalQuestions int) map[string]int {
	var alwaysOneCats, coreCats, normalCats []CategoryDTO
	for _, cat := range categories {
		switch cat.Priority {
		case "ALWAYS_ONE":
			alwaysOneCats = append(alwaysOneCats, cat)
		case "CORE":
			coreCats = append(coreCats, cat)
		default:
			normalCats = append(normalCats, cat)
		}
	}

	allocation := map[string]int{}
	remaining := totalQuestions

	// Phase 1: ALWAYS_ONE 保底各 1 题。
	for _, cat := range alwaysOneCats {
		if remaining > 0 {
			allocation[cat.Key] = 1
			remaining--
		}
	}

	// Phase 2: 先给所有类目各 1 题（CORE 优先），保证覆盖率。
	for _, cat := range coreCats {
		if remaining > 0 {
			allocation[cat.Key] = 1
			remaining--
		}
	}
	for _, cat := range normalCats {
		if remaining > 0 {
			allocation[cat.Key] = 1
			remaining--
		}
	}

	// Phase 3: 剩余名额按 CORE 优先轮转分配。
	for remaining > 0 {
		progressed := false
		for _, cat := range coreCats {
			if remaining <= 0 {
				break
			}
			allocation[cat.Key]++
			remaining--
			progressed = true
		}
		for _, cat := range normalCats {
			if remaining <= 0 {
				break
			}
			allocation[cat.Key]++
			remaining--
			progressed = true
		}
		if !progressed {
			// 对齐 Java：coreCats 与 normalCats 均为空时退出，避免死循环。
			break
		}
	}

	// 确保所有 CORE/NORMAL 类目都出现在 allocation 中（可为 0）。
	for _, cat := range coreCats {
		if _, ok := allocation[cat.Key]; !ok {
			allocation[cat.Key] = 0
		}
	}
	for _, cat := range normalCats {
		if _, ok := allocation[cat.Key]; !ok {
			allocation[cat.Key] = 0
		}
	}
	return allocation
}

// BuildAllocationDescription 分配表转 Markdown（对齐 Java buildAllocationDescription，
// 317-326 行）：按 categories 顺序，数量 > 0 的类目生成 "| label | n 题 | priority |" 行。
func BuildAllocationDescription(allocation map[string]int, categories []CategoryDTO) string {
	var sb []byte
	for _, cat := range categories {
		count := allocation[cat.Key]
		if count > 0 {
			sb = append(sb, ("| " + cat.Label + " | " + strconv.Itoa(count) + " 题 | " + cat.Priority + " |\n")...)
		}
	}
	return string(sb)
}

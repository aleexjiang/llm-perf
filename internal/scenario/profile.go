// profile.go：user 模式的会话形状 profile（scripts/profile_build.py 产出）。
//
// profile 只含统计特征（轮次分布、输入/上下文增量分布、权重），不含任何 trace 消息文本；
// 运行时文本由经典书语料按 seed 生成（见 user.go），负载形状由本文件承载。
package scenario

import (
	"encoding/json"
	"fmt"
	"os"
)

// Profile profile.json 的结构（version 1，见 docs/workload-refactor-plan.md 13.2）。
type Profile struct {
	Version         int                     `json:"version"`
	GeneratedAt     string                  `json:"generated_at"`
	Source          string                  `json:"source"`
	FirstTurnTokens []int                   `json:"first_turn_tokens"` // [min,max]：首轮 prompt 总量硬约束（agent 形状 35-40K）
	Profiles        map[string]*ProfileSpec `json:"profiles"`
	Cleaning        map[string]int          `json:"cleaning"`
	Notes           []string                `json:"notes"`
}

// ProfileSpec 单个会话档位（light/medium/heavy）。
type ProfileSpec struct {
	Weight          float64 `json:"weight"`                   // 运行比例（默认 6:3:1，人工设定）
	TurnsRange      []int   `json:"turns_range"`              // [lo] 或 [lo,hi]；单元素 = lo 为下限
	UserInputTokens []int   `json:"user_input_tokens"`        // [lo,hi] 每轮 user 文本长度（token）
	ContextTokens   []int   `json:"context_tokens"`           // [lo,hi] 每轮注入的合成上下文（token，尾部 <context> 块）
	TraceSessions   int     `json:"trace_sessions,omitempty"` // 特征来源的会话数（参考）
}

// LoadProfile 读取并校验 profile.json。
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("user profile 读取失败: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("user profile 解析失败 %s: %w", path, err)
	}
	if p.Version < 1 {
		return nil, fmt.Errorf("user profile 版本缺失（version=%d）", p.Version)
	}
	if len(p.Profiles) == 0 {
		return nil, fmt.Errorf("user profile 无 profiles 段: %s", path)
	}
	total := 0.0
	for name, spec := range p.Profiles {
		if spec == nil || spec.Weight <= 0 {
			return nil, fmt.Errorf("user profile 档位 %s 权重非法", name)
		}
		if len(spec.TurnsRange) == 0 || spec.TurnsRange[0] < 2 {
			return nil, fmt.Errorf("user profile 档位 %s turns_range 非法（user 模式必须多轮，最少 2 轮）", name)
		}
		if len(spec.UserInputTokens) != 2 || spec.UserInputTokens[0] < 0 || spec.UserInputTokens[1] < spec.UserInputTokens[0] {
			return nil, fmt.Errorf("user profile 档位 %s user_input_tokens 非法", name)
		}
		if len(spec.ContextTokens) != 2 || spec.ContextTokens[0] < 0 || spec.ContextTokens[1] < spec.ContextTokens[0] {
			return nil, fmt.Errorf("user profile 档位 %s context_tokens 非法", name)
		}
		total += spec.Weight
	}
	if total <= 0 {
		return nil, fmt.Errorf("user profile 权重总和为 0")
	}
	if len(p.FirstTurnTokens) != 2 || p.FirstTurnTokens[0] < 35000 {
		// agent 形状硬约束（plan 13.2）：首轮 prompt ≥ 35K token
		return nil, fmt.Errorf("user profile first_turn_tokens 非法：首轮 prompt 必须 ≥35000 token（agent 形状约束），得到 %v", p.FirstTurnTokens)
	}
	return &p, nil
}

// turnBounds 档位轮次上下界（单元素 range = [lo, lo]，由运行时上限另行保护）。
func (s *ProfileSpec) turnBounds(maxTurns int) (int, int) {
	lo := s.TurnsRange[0]
	hi := lo
	if len(s.TurnsRange) > 1 {
		hi = s.TurnsRange[1]
	}
	if maxTurns > 0 && hi > maxTurns {
		hi = maxTurns
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

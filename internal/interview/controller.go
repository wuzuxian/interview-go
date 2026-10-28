package interview

import (
	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/result"
)

// Controller 面试模块控制器，路径与 Java 版 InterviewSkillController 一致。
type Controller struct {
	svc *Service
}

// NewController 创建控制器。
func NewController(svc *Service) *Controller {
	return &Controller{svc: svc}
}

// RegisterRoutes 注册 /api/interview 下本票范围内的全部路由。
// 列表与详情为公开读接口，Java 版无 @RateLimit，故不挂限流（parse-jd 随票 02 上线时再加）。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/interview")

	skills := h.Group("/skills")
	skills.GET("", ctl.listSkills)
	skills.GET("/:id", ctl.getSkill)
}

func (ctl *Controller) listSkills(c *gin.Context) {
	result.OK(c, ctl.svc.ListSkills())
}

func (ctl *Controller) getSkill(c *gin.Context) {
	data, err := ctl.svc.GetSkill(c.Param("id"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

package dice

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	ds "github.com/sealdice/dicescript"
)

// 命运流向 - 期望值偏移系统
//
// 注: COC 检定越低越易成功, 故"轻盈=低期望(更易), 沉重=高期望(更难)"。
//
// 群模式(KP/管理设定, 按群, 持久化于群属性):
//   均衡之骰 = 期望50.5 (D100真实期望, 原版)
//   轻盈之骰 = 期望35 (出目偏低, 检定更易成功)
//   沉重之骰 = 期望65 (出目偏高, 检定更难)
//   窃命之骰 = 管理自定期望
// 个人模式(按 jrrp, 个人自设, 按群隔离, 仅群组为均衡时生效):
//   命运之骰 = 期望为 100-今日人品(人品越高期望越低、越易成功)
//
// 优先级: 群组流向非均衡时一律用群组; 仅群组为均衡(默认)时, 才轮到个人命运。
//
// 注: 期望以 d100 为基准, dicescript 内部会按比例缩放到任意面数。
//
// 持久化: 借助海豹 AttrsManager, 群模式与个人开关均存于群属性(个人开关key带UserID),
// 自动随数据库保存, 重启不丢失, 且个人命运不会跨群。

const (
	fateExpBalance = 50.5 // 均衡(D100真实期望)
	fateExpLight   = 35.0 // 轻盈(低期望, 更易成功)
	fateExpHeavy   = 65.0 // 沉重(高期望, 更难)

	// 持久化属性 key
	fateAttrGroupExp  = "$fate_group_exp"  // 群属性: 群级期望值(float)
	fateAttrGroupName = "$fate_group_name" // 群属性: 群级模式名(str)
	fateAttrPersonal  = "$fate_personal"   // 群属性: 个人命运之骰开关前缀(后接:UserID, int 0/1)
)

// fateLoadGroupExp 从群属性读取群级期望值(0=未设置/均衡)
func fateLoadGroupExp(ctx *MsgContext) float64 {
	if ctx == nil || ctx.Group == nil || ctx.Dice == nil || ctx.Dice.AttrsManager == nil {
		return 0
	}
	attrs, err := ctx.Dice.AttrsManager.LoadById(ctx.Group.GroupID)
	if err != nil || attrs == nil {
		return 0
	}
	v := attrs.Load(fateAttrGroupExp)
	if v == nil {
		return 0
	}
	switch v.TypeId {
	case ds.VMTypeFloat:
		return v.MustReadFloat()
	case ds.VMTypeInt:
		return float64(v.MustReadInt())
	}
	return 0
}

// fateLoadGroupName 从群属性读取群级模式名
func fateLoadGroupName(ctx *MsgContext) string {
	if ctx == nil || ctx.Group == nil || ctx.Dice == nil || ctx.Dice.AttrsManager == nil {
		return "⚖ 均衡之骰(默认)"
	}
	attrs, err := ctx.Dice.AttrsManager.LoadById(ctx.Group.GroupID)
	if err != nil || attrs == nil {
		return "⚖ 均衡之骰(默认)"
	}
	v := attrs.Load(fateAttrGroupName)
	if v == nil || v.TypeId != ds.VMTypeString {
		return "⚖ 均衡之骰(默认)"
	}
	s, _ := v.ReadString()
	if s == "" {
		return "⚖ 均衡之骰(默认)"
	}
	return s
}

// fatePersonalKey 个人命运开关的群属性 key(按 UserID 隔离, 只在当前群生效)
func fatePersonalKey(ctx *MsgContext) string {
	return fateAttrPersonal + ":" + ctx.Player.UserID
}

// fateIsPersonalOn 读取个人命运之骰开关(按群隔离, 存于当前群属性)
func fateIsPersonalOn(ctx *MsgContext) bool {
	if ctx == nil || ctx.Player == nil || ctx.Group == nil || ctx.Dice == nil || ctx.Dice.AttrsManager == nil {
		return false
	}
	attrs, err := ctx.Dice.AttrsManager.LoadById(ctx.Group.GroupID)
	if err != nil || attrs == nil {
		return false
	}
	v := attrs.Load(fatePersonalKey(ctx))
	if v == nil || v.TypeId != ds.VMTypeInt {
		return false
	}
	return v.MustReadInt() != 0
}

// fateSetPersonal 设置个人命运之骰开关(按群隔离, 持久化于当前群属性)
func fateSetPersonal(ctx *MsgContext, on bool) {
	if ctx == nil || ctx.Player == nil || ctx.Group == nil || ctx.Dice == nil || ctx.Dice.AttrsManager == nil {
		return
	}
	attrs, err := ctx.Dice.AttrsManager.LoadById(ctx.Group.GroupID)
	if err != nil || attrs == nil {
		return
	}
	val := ds.IntType(0)
	if on {
		val = 1
	}
	attrs.Store(fatePersonalKey(ctx), ds.NewIntVal(val))
}

// fatePersonalExp 计算个人命运之骰的期望值: 100-今日人品(人品越高期望越低、越易成功)。
// 人品满值100时clamp到1, 避免期望为0被判为未偏移。失败返回0。
func fatePersonalExp(ctx *MsgContext) float64 {
	rp := getTodayJrrp(ctx)
	if rp <= 0 {
		return 0
	}
	exp := 100 - rp
	if exp < 1 {
		exp = 1
	}
	return float64(exp)
}

// GetFateExpectation 返回当前上下文生效的期望值(0=不偏移)。
// 优先级: 群级模式为主; 仅当群级为均衡(默认)时, 才轮到个人命运之骰。
func GetFateExpectation(ctx *MsgContext) float64 {
	if ctx == nil {
		return 0
	}

	// 群级模式优先: 非均衡(已设置且不等于均衡值)时直接采用
	groupExp := fateLoadGroupExp(ctx)
	if groupExp > 0 && groupExp != fateExpBalance {
		return groupExp
	}

	// 群级为均衡或未设置时, 才轮到个人命运之骰
	if fateIsPersonalOn(ctx) {
		return fatePersonalExp(ctx)
	}

	return groupExp
}

// FateRollHint 当命运期望被偏移(非0且非均衡)时, 返回一行命运提示(带前导换行),
// 否则返回空串。用于在各类骰点结果后追加提示。
func FateRollHint(ctx *MsgContext) string {
	exp := GetFateExpectation(ctx)
	if exp <= 0 || exp == fateExpBalance {
		return ""
	}
	return "\n「注意：命运已被拨动·当前期望" + strconv.FormatFloat(exp, 'f', -1, 64) + "」"
}

// getTodayJrrp 复现 jrrp(今日人品) 的算法, 返回 1-100。失败返回 0。
// 算法与 ext_fun.go 的 .jrrp 指令保持一致, 保证同一玩家当天数值相同。
func getTodayJrrp(ctx *MsgContext) int64 {
	if ctx == nil || ctx.Player == nil || ctx.EndPoint == nil {
		return 0
	}
	rpSeed := (time.Now().Unix() + (8 * 60 * 60)) / (24 * 60 * 60)
	rpSeed += int64(fingerprint(ctx.EndPoint.UserID))
	rpSeed += int64(fingerprint(ctx.Player.UserID))
	randItem := rand.NewSource(rpSeed)
	rp := randItem.Int63()%100 + 1
	if rp < 1 {
		rp = 1
	}
	if rp > 100 {
		rp = 100
	}
	return rp
}

// fateSetGroupMode 设置群级模式(持久化于群属性)
func fateSetGroupMode(ctx *MsgContext, mode string, customExp float64) (string, bool) {
	if ctx == nil || ctx.Group == nil || ctx.Dice == nil || ctx.Dice.AttrsManager == nil {
		return "", false
	}
	attrs, err := ctx.Dice.AttrsManager.LoadById(ctx.Group.GroupID)
	if err != nil || attrs == nil {
		return "", false
	}

	var exp float64
	var name string
	switch mode {
	case "均衡", "均衡之骰", "balance":
		exp, name = fateExpBalance, "⚖ 均衡之骰"
	case "轻盈", "轻盈之骰", "light":
		exp, name = fateExpLight, "☁ 轻盈之骰"
	case "沉重", "沉重之骰", "heavy":
		exp, name = fateExpHeavy, "⛰ 沉重之骰"
	case "窃命", "窃命之骰", "custom":
		if customExp < 1 || customExp > 100 {
			return "", false
		}
		exp = customExp
		name = "👑 窃命之骰(" + strconv.FormatFloat(customExp, 'f', -1, 64) + ")"
	default:
		return "", false
	}

	attrs.Store(fateAttrGroupExp, ds.NewFloatVal(exp))
	attrs.Store(fateAttrGroupName, ds.NewStrVal(name))
	return name, true
}

// fateFormatStatus 组装命运流向状态文本
func fateFormatStatus(ctx *MsgContext) string {
	var b strings.Builder
	b.WriteString("■ 命运流向——命运之匣的权能＼＼＼\n")
	b.WriteString("——————————\n")

	b.WriteString("｜群组流向：" + fateLoadGroupName(ctx) + "\n")

	personalText := "未启用"
	if fateIsPersonalOn(ctx) {
		rp := getTodayJrrp(ctx)
		personalText = "🔮 命运之骰(今日人品=" + strconv.FormatInt(rp, 10) + ")"
	}
	b.WriteString("｜个人命运：" + personalText + "\n")

	exp := GetFateExpectation(ctx)
	if exp <= 0 || exp == fateExpBalance {
		b.WriteString("｜当前期望：" + strconv.FormatFloat(fateExpBalance, 'f', -1, 64) + "(均衡)\n")
	} else {
		b.WriteString("｜当前期望：" + strconv.FormatFloat(exp, 'f', -1, 64) + "\n")
	}
	b.WriteString("——————————\n")
	b.WriteString("「骰之轻重，皆在命运之匣的掌控之中」")
	return b.String()
}

// RegisterBuiltinExtFate 注册「命运流向」内置扩展
func RegisterBuiltinExtFate(dice *Dice) {
	helpFate := "■ 命运流向 - 调整骰子期望值＼＼＼\n" +
		"通过偏态分布改变所有骰子的出目期望，影响掷骰手感与检定成败。\n" +
		"——————————\n" +
		"｜群组流向(需KP/管理权限)：\n" +
		".fate 均衡 // ⚖ 期望50.5，原版均匀分布\n" +
		".fate 轻盈 // ☁ 期望35，出目偏低，检定更易成功\n" +
		".fate 沉重 // ⛰ 期望65，出目偏高，检定更难\n" +
		".fate 窃命 <1-100> // 👑 自定期望，如 .fate 窃命 80\n" +
		"——————————\n" +
		"｜个人命运(任何人可设，仅群组为均衡时生效)：\n" +
		".fate 命运 // 🔮 期望随今日人品流转，人品越高越易成功\n" +
		".fate 命运 off // 关闭个人命运之骰\n" +
		"——————————\n" +
		".fate 状态 // 查看当前命运流向状态\n" +
		"「骰之轻重，皆在命运之匣的掌控之中」"

	cmdFate := &CmdItemInfo{
		Name:      "fate",
		ShortHelp: helpFate,
		Help:      helpFate,
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			arg1 := cmdArgs.GetArgN(1)

			// 无参数 -> 显示帮助文档(像其他插件一样)
			if arg1 == "" {
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}

			if cmdArgs.IsArgEqual(1, "help") {
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}

			// 状态查询
			if arg1 == "状态" || arg1 == "status" {
				ReplyToSender(ctx, msg, fateFormatStatus(ctx))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			if ctx.IsPrivate || ctx.Group == nil {
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 个人命运之骰: .fate 命运 [off]
			if arg1 == "命运" || arg1 == "命运之骰" {
				arg2 := cmdArgs.GetArgN(2)
				if arg2 == "off" || arg2 == "关" || arg2 == "关闭" {
					fateSetPersonal(ctx, false)
					ReplyToSender(ctx, msg, "■ 命运流向已切换\n｜个人命运：🔮 命运之骰 已熄灭\n｜你的骰子回归群组流向\n——————————\n「命运之匣收回了为你单独校准的权能」")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				fateSetPersonal(ctx, true)
				rp := getTodayJrrp(ctx)
				exp := fatePersonalExp(ctx)
				ReplyToSender(ctx, msg, fmt.Sprintf("■ 命运流向已切换\n｜当前个人命运：🔮 命运之骰\n｜今日命运之数：%d\n｜骰子期望：%s\n——————————\n「命运之匣已为你单独校准，骰之轻重随今日命数流转」\n注：仅当群组流向为均衡时，个人命运方才生效", rp, strconv.FormatFloat(exp, 'f', -1, 64)))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 群级模式设定，需要权限
			if ctx.PrivilegeLevel < 50 {
				ReplyToSender(ctx, msg, "调整群组命运流向需要KP/管理权限。\n你仍可使用 .fate 命运 启用个人命运之骰。")
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			var customExp float64
			if arg1 == "窃命" || arg1 == "窃命之骰" || arg1 == "custom" {
				expStr := cmdArgs.GetArgN(2)
				v, err := strconv.ParseFloat(expStr, 64)
				if err != nil || v < 1 || v > 100 {
					ReplyToSender(ctx, msg, "窃命之骰需指定期望值(1-100)，如 .fate 窃命 80")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				customExp = v
			}

			modeName, ok := fateSetGroupMode(ctx, arg1, customExp)
			if !ok {
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}

			exp := fateLoadGroupExp(ctx)
			ReplyToSender(ctx, msg, fmt.Sprintf("■ 命运流向已切换\n｜当前群组流向：%s\n｜骰子期望：%s\n——————————\n「命运之匣已重新校准，骰之轻重就此改变」", modeName, strconv.FormatFloat(exp, 'f', -1, 64)))
			return CmdExecuteResult{Matched: true, Solved: true}
		},
	}

	theExt := &ExtInfo{
		Name:        "fate",
		Aliases:     []string{"命运流向", "命运之骰"},
		Version:     "1.0.0",
		Brief:       "命运流向，通过偏态分布调整所有骰子的期望值，可设置群组流向(均衡/轻盈/沉重/窃命)与个人命运之骰(按今日人品)。",
		Author:      "命运之匣",
		AutoActive:  true,
		Official:    false,
		GetDescText: GetExtensionDesc,
	}
	theExt.CmdMap = map[string]*CmdItemInfo{
		"fate": cmdFate,
		"命运":   cmdFate,
		"命运流向": cmdFate,
		"命运之骰": cmdFate,
	}
	dice.RegisterExtension(theExt)
}
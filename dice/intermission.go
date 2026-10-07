package dice

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	ds "github.com/sealdice/dicescript"
	"github.com/tidwall/gjson"

	"sealdice-core/dice/service"
)

// 幕间成长 - 对玩家在一段 log 内检定成功的技能进行批量成长(COC7)
//
// 两种数据来源:
//   .幕间 成长            -> 读取本群最近一段已结束 log(本地数据库)
//   .幕间 <log链接> [角色名] -> 从染色器链接下载该 log
//   .幕间 确认            -> 对暂存的可成长技能逐个成长(d100>当前值或>95即成功, +1d10)
//   .幕间 取消            -> 取消
//
// 成长规则与 .en 一致, 仅 COC7。
// 暂存于内存(带 TTL), 按 群:用户 隔离, 重启失效。

// 不可成长项(检定成功也不成长): 属性值/信用/克苏鲁神话
var interNonGrow = map[string]bool{
	"力量": true, "敏捷": true, "体质": true, "体型": true, "外貌": true,
	"智力": true, "意志": true, "教育": true, "幸运": true, "理智": true,
	"生命值": true, "魔法值": true, "san": true, "hp": true, "mp": true,
	"db": true, "体格": true, "移动": true, "移动力": true, "护甲": true,
	"信用": true, "信誉": true, "信用评级": true,
	"克苏鲁神话": true, "克苏鲁": true, "cm": true,
}

// 幕间暂存
type interPending struct {
	skills []string
	ts     time.Time
}

var (
	interPendingMap = map[string]*interPending{}
	interPendingMu  sync.Mutex
)

const interPendingTTL = 10 * time.Minute

func interPendingKey(ctx *MsgContext) string {
	g := "private"
	if ctx.Group != nil {
		g = ctx.Group.GroupID
	}
	uid := ""
	if ctx.Player != nil {
		uid = ctx.Player.UserID
	}
	return g + ":" + uid
}

func interSetPending(ctx *MsgContext, skills []string) {
	interPendingMu.Lock()
	defer interPendingMu.Unlock()
	interPendingMap[interPendingKey(ctx)] = &interPending{skills: skills, ts: time.Now()}
}

func interGetPending(ctx *MsgContext) []string {
	interPendingMu.Lock()
	defer interPendingMu.Unlock()
	v := interPendingMap[interPendingKey(ctx)]
	if v == nil {
		return nil
	}
	if time.Since(v.ts) > interPendingTTL {
		delete(interPendingMap, interPendingKey(ctx))
		return nil
	}
	return v.skills
}

func interClearPending(ctx *MsgContext) {
	interPendingMu.Lock()
	defer interPendingMu.Unlock()
	delete(interPendingMap, interPendingKey(ctx))
}

// interNormalizeSkill 去掉技能名末尾数字
func interNormalizeSkill(s string) string {
	s = strings.TrimSpace(s)
	end := len(s)
	for i, r := range s {
		if r >= '0' && r <= '9' {
			end = i
			break
		}
	}
	if end < len(s) {
		s = s[:end]
	}
	return strings.TrimSpace(s)
}

func interIsNonGrow(skill string) bool {
	s := interNormalizeSkill(skill)
	if interNonGrow[s] {
		return true
	}
	if interNonGrow[strings.ToLower(s)] {
		return true
	}
	return false
}

// interCollectSkills 从 log 的检定记录(commandInfo JSON 字符串列表)聚合目标玩家的成功/失败技能。
func interCollectSkills(ctx *MsgContext, items []string, targetName string) (map[string]int, map[string]int) {
	successSet := map[string]int{}
	failedSet := map[string]int{}

	var tmpl *GameSystemTemplate
	if ctx.Group != nil {
		tmpl = ctx.Group.GetCharTemplate(ctx.Dice)
	}
	getName := func(s string) string {
		s = interNormalizeSkill(s)
		if tmpl != nil {
			return tmpl.GetAlias(s)
		}
		return s
	}

	for _, raw := range items {
		info := gjson.Parse(raw)
		if info.Get("rule").String() != "coc7" {
			continue
		}
		if info.Get("cmd").String() != "ra" {
			continue
		}
		pcName := info.Get("pcName").String()
		if targetName != "" && !strings.Contains(pcName, targetName) && !strings.Contains(targetName, pcName) {
			continue
		}
		if !info.Get("items").IsArray() {
			continue
		}
		for _, j := range info.Get("items").Array() {
			rank := j.Get("rank").Float()
			attr := getName(j.Get("expr2").String())
			if attr == "" {
				continue
			}
			if rank > 0 {
				successSet[attr]++
			} else if rank < 0 {
				failedSet[attr]++
			}
		}
	}
	return successSet, failedSet
}

// interClassify 分类: 可成长 / 属性特殊 / 仅失败
func interClassify(successSet, failedSet map[string]int) (growable, blocked, failedOnly []string) {
	for skill := range successSet {
		if interIsNonGrow(skill) {
			blocked = append(blocked, skill)
		} else {
			growable = append(growable, skill)
		}
	}
	for fskill := range failedSet {
		if successSet[fskill] == 0 {
			failedOnly = append(failedOnly, fskill)
		}
	}
	return
}

// interGrowOne 对单个技能成长(规则同 .en): d100>当前值或>95即成功, 成功+1d10。
// 返回 (旧值, d100, 是否成功, 增量, 新值, 是否有效)
func interGrowOne(ctx *MsgContext, skill string) (int64, int64, bool, int64, int64, bool) {
	val, exists := VarGetValue(ctx, skill)
	if !exists {
		return 0, 0, false, 0, 0, false
	}
	if val.TypeId != ds.VMTypeInt {
		return 0, 0, false, 0, 0, false
	}
	varValue := int64(val.MustReadInt())

	d100 := DiceRoll64(100)
	cocRule := 0
	if ctx.Group != nil {
		cocRule = ctx.Group.CocRuleIndex
	}
	successRank, _ := ResultCheck(ctx, cocRule, d100, varValue, 0)
	if d100 > 95 {
		successRank = -1
	}
	success := successRank <= 0
	if !success {
		return varValue, d100, false, 0, varValue, true
	}
	inc := DiceRoll64(10)
	newVal := varValue + inc
	VarSetValueInt64(ctx, skill, newVal)
	return varValue, d100, true, inc, newVal, true
}

// ============ 染色器链接下载 ============

var interKeyRe = regexp.MustCompile(`[?&]key=([^&#]+)`)
var interPwdRe = regexp.MustCompile(`#(\d+)`)

// interParseLogURL 从染色器链接提取 key 与 password
func interParseLogURL(url string) (key, password string) {
	if m := interKeyRe.FindStringSubmatch(url); len(m) > 1 {
		key = m[1]
	}
	if m := interPwdRe.FindStringSubmatch(url); len(m) > 1 {
		password = m[1]
	}
	return
}

// interFetchLogItems 从染色器下载 log, 返回 commandInfo JSON 字符串列表与 log 名。
// 染色器返回 { name, data(base64(zlib(json))) }, json 内 items 为 LogOneItem 列表。
func interFetchLogItems(key, password string) (items []string, logName string, errMsg string) {
	apiURL := "https://dice-api.weizaima.com/dice/api/load_data?key=" + key + "&password=" + password

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, "", "网络请求失败：" + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "获取Log数据失败（HTTP " + strconv.Itoa(resp.StatusCode) + "）"
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "读取响应失败：" + err.Error()
	}

	root := gjson.ParseBytes(body)
	dataB64 := root.Get("data").String()
	if dataB64 == "" {
		return nil, "", "Log数据为空（data字段缺失）"
	}
	logName = root.Get("name").String()
	if logName == "" {
		logName = "未命名"
	}

	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, "", "base64解码失败：" + err.Error()
	}

	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, "", "数据解压初始化失败：" + err.Error()
	}
	defer zr.Close()
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		return nil, "", "数据解压失败：" + err.Error()
	}

	logJSON := gjson.ParseBytes(decompressed)
	logItems := logJSON.Get("items")
	if !logItems.IsArray() {
		return nil, logName, "解压后的数据格式异常（items 缺失）"
	}

	// 提取每条记录的 commandInfo, 转成 JSON 字符串(与 service.LogGetCommandInfoStrList 对齐)
	logItems.ForEach(func(_, item gjson.Result) bool {
		ci := item.Get("commandInfo")
		if !ci.Exists() {
			ci = item.Get("CommandInfo")
		}
		if ci.Exists() && ci.Raw != "" && ci.Raw != "null" {
			items = append(items, ci.Raw)
		}
		return true
	})
	return items, logName, ""
}

// ============ 结算文本 ============

// interBuildResult 执行成长并组装结算文本
func interBuildResult(ctx *MsgContext, skills []string) string {
	var lines []string
	grown := 0
	for _, skill := range skills {
		oldVal, d100, success, inc, newVal, valid := interGrowOne(ctx, skill)
		if !valid {
			lines = append(lines, "｜〖"+skill+"〗未录入，跳过")
			continue
		}
		if success {
			grown++
			lines = append(lines, "｜〖"+skill+"〗成长检定：D100="+strconv.FormatInt(d100, 10)+" ＞ "+strconv.FormatInt(oldVal, 10)+"〖"+strconv.FormatInt(oldVal, 10)+"→"+strconv.FormatInt(newVal, 10)+"，+"+strconv.FormatInt(inc, 10)+"〗")
		} else {
			lines = append(lines, "｜〖"+skill+"〗成长检定：D100="+strconv.FormatInt(d100, 10)+" ≤ "+strconv.FormatInt(oldVal, 10)+"〖未成长〗")
		}
	}

	if ctx.Player.AutoSetNameTemplate != "" {
		_, _ = SetPlayerGroupCardByTemplate(ctx, ctx.Player.AutoSetNameTemplate)
	}

	return "■ 命运之匣 · 幕间成长\n｜成长已结算＼\n——————————\n" +
		strings.Join(lines, "\n") +
		"\n——————————\n｜共〖" + strconv.Itoa(len(skills)) + "〗项检定，〖" + strconv.Itoa(grown) + "〗项获得成长\n「往昔的试炼，已化作你前路的力量」"
}

// interBuildPreview 组装"检索结果"展示文本, 并把可成长技能暂存。
func interBuildPreview(ctx *MsgContext, targetName, logName string, growable, blocked, failedOnly []string) string {
	interSetPending(ctx, growable)

	var b strings.Builder
	b.WriteString("■ 命运之匣 · 幕间\n｜检索到〖" + targetName + "〗在故事〖" + logName + "〗中的检定＼\n")
	b.WriteString("——————————\n")
	b.WriteString("｜可成长（检定成功）：\n")
	if len(growable) > 0 {
		b.WriteString("｜　〖" + strings.Join(growable, "〗〖") + "〗\n")
	} else {
		b.WriteString("｜　〖无〗\n")
	}
	b.WriteString("——————————\n")
	b.WriteString("｜无法成长：\n")
	var parts []string
	if len(blocked) > 0 {
		parts = append(parts, "〖"+strings.Join(blocked, "〗〖")+"〗（属性/特殊）")
	}
	if len(failedOnly) > 0 {
		parts = append(parts, "〖"+strings.Join(failedOnly, "〗〖")+"〗（检定失败）")
	}
	if len(parts) > 0 {
		b.WriteString("｜　" + strings.Join(parts, "；") + "\n")
	} else {
		b.WriteString("｜　〖无〗\n")
	}
	b.WriteString("——————————\n")
	if len(growable) > 0 {
		b.WriteString("｜回复〖.幕间 确认〗即可对成长技能批量成长\n｜回复〖.幕间 取消〗放弃本次成长\n")
	} else {
		b.WriteString("｜没有可供成长的技能\n")
		interClearPending(ctx)
	}
	b.WriteString("「往昔的试炼，终将化作前路的力量」")
	return b.String()
}

// RegisterBuiltinExtIntermission 注册「幕间成长」内置扩展(本地 log + 染色器链接)
func RegisterBuiltinExtIntermission(dice *Dice) {
	helpInter := "■ 命运之匣 · 幕间\n" +
		"｜于故事的间隙，沉淀成长＼\n" +
		"——————————\n" +
		"｜对你在一段故事中检定成功的技能进行批量成长(COC7)\n" +
		"——————————\n" +
		".幕间 成长 // 读取本群上一段已结束 log\n" +
		".幕间 <log链接> [角色名] // 从染色器链接读取(角色名按log内昵称)\n" +
		".幕间 确认 // 对上一步列出的可成长技能批量成长\n" +
		".幕间 取消 // 取消本次幕间成长\n" +
		"——————————\n" +
		"｜成长规则：检定成功的技能可成长，d100 大于当前值则 +1d10\n" +
		"｜不可成长：属性值、信用评级、克苏鲁神话，以及失败的技能\n" +
		"｜成长结果写入当前绑定的角色卡\n" +
		"「往昔的试炼，终将化作前路的力量」"

	cmdInter := &CmdItemInfo{
		Name:      "幕间",
		ShortHelp: helpInter,
		Help:      helpInter,
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			arg1 := cmdArgs.GetArgN(1)

			if arg1 == "" || arg1 == "help" {
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}

			if ctx.IsPrivate || ctx.Group == nil {
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 取消
			if arg1 == "取消" || arg1 == "cancel" {
				if interGetPending(ctx) != nil {
					interClearPending(ctx)
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜本次幕间成长已取消\n「成长之路，何时启程皆由你」")
				} else {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜当前没有待确认的幕间成长")
				}
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 确认 -> 执行成长
			if arg1 == "确认" || arg1 == "confirm" {
				skills := interGetPending(ctx)
				if skills == nil {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜没有待确认的成长，请先〖.幕间 成长〗或〖.幕间 <log链接>〗")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				if len(skills) == 0 {
					interClearPending(ctx)
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜没有可成长的技能")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				out := interBuildResult(ctx, skills)
				interClearPending(ctx)
				ReplyToSender(ctx, msg, out)
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 成长 -> 读本地 log
			if arg1 == "成长" || arg1 == "grow" {
				if ctx.Group.LogOn {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜当前 log 仍在记录中\n｜请先〖.log off〗或〖.log end〗结束记录后再进行幕间成长")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				logName := ctx.Group.LogCurName
				if logName == "" {
					lst, err := service.LogGetList(ctx.Dice.DBOperator, ctx.Group.GroupID)
					if err == nil && len(lst) > 0 {
						logName = lst[0] // updated_at DESC, 第一个为最近
					}
				}
				if logName == "" {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜未找到本群的 log 记录\n｜请确认曾用〖.log new〗/〖.log end〗记录过故事")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				items, err := service.LogGetCommandInfoStrList(ctx.Dice.DBOperator, ctx.Group.GroupID, logName)
				if err != nil || len(items) == 0 {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜log〖"+logName+"〗内没有可供成长的检定记录")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				targetName := ctx.Player.Name
				successSet, failedSet := interCollectSkills(ctx, items, targetName)
				growable, blocked, failedOnly := interClassify(successSet, failedSet)

				if len(growable) == 0 && len(blocked) == 0 && len(failedOnly) == 0 {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜在 log〖"+logName+"〗中未检测到〖"+targetName+"〗的检定记录\n｜请确认角色名与 log 内一致")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				ReplyToSender(ctx, msg, interBuildPreview(ctx, targetName, logName, growable, blocked, failedOnly))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// 否则 arg1 视为 log 链接
			if strings.Contains(arg1, "key=") {
				key, password := interParseLogURL(arg1)
				if key == "" {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜异常捕获——无法从链接提取key［！］")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				targetName := cmdArgs.GetArgN(2)
				if targetName == "" {
					targetName = ctx.Player.Name
				}

				ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜正在翻阅往昔的记录＼＼＼\n｜目标角色：〖"+targetName+"〗\n「请稍候，命运之匣正在检索」")

				items, logName, errMsg := interFetchLogItems(key, password)
				if errMsg != "" {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜异常捕获——"+errMsg+"［！］")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				if len(items) == 0 {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜log〖"+logName+"〗内没有结构化的检定记录\n｜（该链接可能未保留检定数据）")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				successSet, failedSet := interCollectSkills(ctx, items, targetName)
				growable, blocked, failedOnly := interClassify(successSet, failedSet)

				if len(growable) == 0 && len(blocked) == 0 && len(failedOnly) == 0 {
					ReplyToSender(ctx, msg, "■ 命运之匣 · 幕间\n｜在 log〖"+logName+"〗中未检测到〖"+targetName+"〗的检定记录\n｜请确认角色名与 log 内昵称一致")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				ReplyToSender(ctx, msg, interBuildPreview(ctx, targetName, logName, growable, blocked, failedOnly))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
		},
	}

	theExt := &ExtInfo{
		Name:        "intermission",
		Aliases:     []string{"幕间", "幕间成长"},
		Version:     "1.1",
		Brief:       "幕间成长，读取本群本地 log 或染色器链接，对当前玩家检定成功的技能进行批量成长(COC7)。",
		Author:      "命运之匣",
		AutoActive:  true,
		Official:    false,
		GetDescText: GetExtensionDesc,
	}
	theExt.CmdMap = map[string]*CmdItemInfo{
		"幕间":   cmdInter,
		"幕间成长": cmdInter,
	}
	dice.RegisterExtension(theExt)
}
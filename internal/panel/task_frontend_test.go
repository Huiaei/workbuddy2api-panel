package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTaskFrontendCapabilitiesAndResults(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('function checkinResultNote('),end=src.indexOf('// 可自动完成的任务');
assert(start>=0&&end>start);
const ctx={AUTO_TASKS:{chat_5:'chat'},GROWTH_TITLES:{}};
vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
const global=ctx.taskRowPresentation({task_code:'first_chat',code:'first_chat',task_id:1,status:'available',progress_known:false,reward_known:false},{accept:false,claim:false,automate:false});
assert.equal(global.code,'first_chat');assert.equal(global.progress,'未提供');assert.equal(global.reward,'未提供');assert.equal(global.action,'');
const cn=ctx.taskRowPresentation({task_code:'chat_5',current:0,target:5,accept_status:'accepted'},{accept:true,claim:true,automate:true});
assert.equal(cn.progress,'0 / 5');assert.equal(cn.action,'auto');
assert.equal(ctx.taskRowPresentation({task_code:'',target:5},{accept:true,claim:true,automate:true}).action,'');
assert.equal(ctx.checkinResultNote({checkin_done:false,checkin_error:'活动未开启',credits:350}).severity,'err');
assert.notEqual(ctx.checkinResultNote({skipped:true,skip_reason:'国际区不适用'}).severity,'ok');
assert.equal(ctx.checkinResultNote({checkin_done:true,credits:350}).severity,'ok');
// balance_error 必须**优先于** checkin_done 展示：企业账号没有签到体系
// （checkin_done 恒为 false、checkin_error 是固定解释性文案），若先判
// checkin_done，用户永远看不到余额查询真正失败的原因（issue #222 建议 3
// 引入判空后，失败原因变得重要起来）。
const balErr=ctx.checkinResultNote({checkin_done:false,checkin_error:'企业账号无签到体系',balance_error:'upstream returned empty quota payload'});
assert(balErr.message.includes('余额刷新失败'),'balance_error 优先展示，实际: '+balErr.message);
assert(balErr.message.includes('empty quota payload'),'balance_error 原文要透出，实际: '+balErr.message);
assert.equal(balErr.severity,'err');
// 签到真的完成但余额刷新失败：仍要显示余额失败。
const balErr2=ctx.checkinResultNote({checkin_done:true,balance_error:'upstream returned null data'});
assert(balErr2.message.includes('余额刷新失败'),'签到完成+余额失败应显示余额失败，实际: '+balErr2.message);
 // **跳过态不是错误**：企业账号没有签到体系（checkin_done 恒为 false、
 // checkin_message 是固定解释性文案），若当成错误渲染，一排健康的企业账号
 // 会全显示成红色「签到未完成」（外部脚本与旧缓存前端都会中招）。
 const skipped=ctx.checkinResultNote({checkin_done:false,checkin_skipped:true,checkin_message:'企业账号无签到体系（已跳过签到，仅刷新额度）',credits:1187});
 assert.notEqual(skipped.severity,'err','跳过态不应渲染成错误，实际: '+skipped.severity);
 assert(skipped.message.includes('企业账号无签到体系'),'跳过态要说明原因，实际: '+skipped.message);
 // 兜底：后端还没升级（没有 checkin_skipped 字段）时，只要是企业号 +
 // 有解释性文案 + 没有真错误，也不该渲染成 err。
 const legacyEnt=ctx.checkinResultNote({enterprise:true,checkin_done:false,checkin_message:'企业账号无签到体系（已跳过签到，仅刷新额度）'});
 assert.notEqual(legacyEnt.severity,'err','旧后端的企业跳过态不应渲染成错误，实际: '+legacyEnt.severity);
 // 但**真的失败**（有 checkin_error）仍然必须是 err。
 assert.equal(ctx.checkinResultNote({enterprise:true,checkin_done:false,checkin_error:'上游返回异常'}).severity,'err');
const summary=ctx.taskResultSummary([{status:'done'},{status:'awaiting_progress'},{status:'done',claim_error:'denied'},{status:'skipped'},{status:'error'}]);
assert(summary.message.includes('已完成 1 项'));assert(summary.message.includes('等待计分 1 项'));assert(summary.message.includes('领奖待重试 1 项'));assert.equal(summary.severity,'err');
const gs=src.indexOf('function groupItems(d)'),ge=src.indexOf('const ST_WORDS');
vm.runInContext(src.slice(gs,ge),ctx);
const groups=ctx.groupItems({accounts:[{uid:'global',skipped:true,skip_reason:'国际区只读'},{uid:'bad',growth_error:'upstream unavailable'}]});
assert.equal(groups.length,2);assert.equal(groups[0].rows[0].status,'skipped');assert.equal(groups[1].rows[0].status,'error');
assert(!src.includes('全部账号的成长任务与开学季活动都已完成'));
console.log('task frontend regression checks passed');`
	path := filepath.Join(t.TempDir(), "task-ui.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

func TestConfigSaveKeepsEnvironmentManagedAuthentication(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf("$('cfgForm').onsubmit ="),end=src.indexOf('/* ── 添加账号');
assert(start>=0&&end>start);
(async()=>{
 for(const managed of [true,false]) {
  const hosts={cfgForm:{},btnCfgSave:{},cfgKey:{value:'submitted-key'},cfgNote:{}};
  let saved='environment-key';
  const ctx={$:id=>hosts[id],DURATION_FIELDS:[],durationBad:()=>false,markDurationFields(){},collectConfig:()=>({api_key:'submitted-key'}),
   api:async()=>({restart_required:[],api_key_env_managed:managed}),
   localStorage:{setItem:(_,value)=>{saved=value}},LS_KEY:'key',toast(){},loadOverview(){},
   loadConfig:async()=>{assert.equal(saved,managed?'environment-key':'submitted-key')},JSON};
  vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
  await hosts.cfgForm.onsubmit({preventDefault(){}});
  assert.equal(saved,managed?'environment-key':'submitted-key');
 }
 console.log('config authentication regression passed');
})().catch(e=>{console.error(e);process.exit(1)});`
	path := filepath.Join(t.TempDir(), "config-ui.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 实时任务卡的前端测试夹具：用真实的 app.js 片段（任务行渲染、实时任务块、
// reattachQueueView、go）在 node 沙箱里跑。各用例只写 body，夹具提供
// liveReply / apiFail / timers / timeouts / calls / nodes / doc / vis / flush / liveCalls。
//
// 切片一律前置 'use strict'：app.js 头部就是严格模式，浏览器里未声明的赋值会直接抛，
// 而 vm 里切出的片段不含文件头的 'use strict'，不带就会放过这类错误（见 frontend_test.go
// 里同类注释）。切片锚点按本仓 app.js 的实际文本取，与 reporter 的锚点不同。
const liveNodePrelude = `'use strict';
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const slice = (from, to) => {
  const a = src.indexOf(from), b = src.indexOf(to, a);
  assert(a > 0 && b > a, '切片锚点失效：' + from + ' → ' + to);
  return src.slice(a, b);
};
const nodes = new Map(), timers = new Map(), timeouts = new Map(), calls = [], vis = [];
let tick = 0, liveReply = {accounts: []}, innerWrites = 0, apiFail = false;
const makeNode = id => {
  const n = {
    textContent: '', hidden: false, disabled: false, value: '', style: {}, dataset: {},
    classList: { add() {}, remove() {}, toggle() {} },
    addEventListener() {}, onclick: null, onchange: null,
    querySelector: () => ({ textContent: '' }),
  };
  let html = '';
  Object.defineProperty(n, 'innerHTML', { get: () => html, set: v => { html = v; if (id === 'liveList') innerWrites++; } });
  return n;
};
const doc = {
  hidden: false,
  addEventListener: (type, fn) => { if (type === 'visibilitychange') vis.push(fn); },
  querySelectorAll: () => [],
};
const ctx = {
  console, Promise, Object, Array, String, Number, Map, JSON, Set, Boolean, Date, Math, encodeURIComponent,
  document: doc, view: 'accounts', TITLES: { accounts: '账号池', taskscenter: '任务中心' }, GROWTH_TITLES: {},
  $: id => { if (!nodes.has(id)) nodes.set(id, makeNode(id)); return nodes.get(id); },
  api: path => {
    calls.push(path);
    if (apiFail) return Promise.reject(new Error('boom'));
    return Promise.resolve(path.startsWith('tasks/live') ? liveReply : {});
  },
  esc: s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
  toast: () => {}, openTasks: () => {},
  loadModels() {}, loadConfig() {}, loadLogs() {}, loadUsage() {}, loadPackages() {}, loadTaskJobs() {},
  queueTimer: null, lastQueueSeq: 0,
  setInterval: (fn, ms) => { const id = ++tick; timers.set(id, { fn, ms }); return id; },
  clearInterval: id => timers.delete(id),
  setTimeout: (fn, ms) => { const id = ++tick; timeouts.set(id, { fn, ms }); return id; },
  clearTimeout: id => timeouts.delete(id),
};
vm.createContext(ctx);
vm.runInContext("'use strict';" + slice('const ST_WORDS = {', 'function renderQueue('), ctx);
vm.runInContext("'use strict';" + slice('/* ── 实时任务', '/* ── 用量'), ctx);
vm.runInContext("'use strict';" + slice('function reattachQueueView()', '/* ── 实时任务'), ctx);
vm.runInContext("'use strict';" + slice('function go(v) {', "document.querySelectorAll('.nav a').forEach(a => a.onclick"), ctx);
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
const liveCalls = () => calls.filter(p => p.startsWith('tasks/live')).length;
`

// runLiveNode 在夹具后接上用例脚本并执行；没有 node 时跳过（与其它前端测试一致）。
func runLiveNode(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := liveNodePrelude + "\n(async () => {\n" + body + "\n})().catch(err => { console.error(err); process.exitCode = 1; });"
	path := filepath.Join(t.TempDir(), name+".cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
}

// 默认 2 分钟一轮（一轮 = 每个启用账号一次上游 GET），所有档位都不低于后端缓存 TTL；
// 轮询的 tick 不得改写用户选的间隔（上游把间隔抬到 TTL 却不重建定时器，只改了变量）。
func TestAppJSLiveTasksDefaultInterval(t *testing.T) {
	runLiveNode(t, "live-interval", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.deepStrictEqual([...timers.values()].map(t => t.ms), [120000], '默认 2 分钟一轮');
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120);
  const choices = vm.runInContext('LIVE_EVERY_CHOICES', ctx);
  assert(choices.every(s => s === 0 || s >= 60), '档位不低于后端缓存 TTL：' + choices);
  [...timers.values()][0].fn(); await flush();
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120, 'tick 不得改写间隔');
  assert.strictEqual(liveCalls(), 2, 'tick 拉一次');

  // 「关」：定时器撤掉，间隔归零
  nodes.get('liveEvery').onchange({ target: { value: '0' } });
  assert.strictEqual(timers.size, 0, '关掉后不该留定时器');
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 0);
  // 非法档位回落到 0（Number('x')||0），不产生 NaN 定时器
  nodes.get('liveEvery').onchange({ target: { value: 'abc' } });
  assert.strictEqual(timers.size, 0);
`)
}

// 任务行必须真的渲染出内容：liveTaskRow 曾返回 qrowHTML 的 HTML，渲染时又被当作
// 行对象再喂一次 qrowHTML，整行变成空白与 "undefined"。
func TestAppJSLiveTaskRowsRender(t *testing.T) {
	runLiveNode(t, "live-rows", `
  liveReply = {age_sec: 0, accounts: [
    {uid: 'cn-1', nickname: 'n1', pending: 1, claimable: 0, claimed: 1, tasks: [
      {task_code: 'chat_5', title: '发起 5 次对话', progress_known: true, current: 2, target: 5, credit: 100},
      {task_code: 'first_buddy', title: '领取一只 Buddy', claimed: true}]},
    {uid: 'cn-2', nickname: 'n2', error: 'token expired'}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const html = nodes.get('liveList').innerHTML;
  assert(html.includes('chat_5') && html.includes('发起 5 次对话') && html.includes('2/5'), '任务行要有内容：' + html);
  assert(!html.includes('undefined'), '任务行不应出现 undefined：' + html);
  assert(html.includes('data-live-uid="cn-1"') && html.includes('token expired'), html);
  assert(nodes.get('liveEmpty').style.display === 'none', '有数据就不该显示空态');
  assert(nodes.get('liveSummary').textContent.includes('2 个账号'), nodes.get('liveSummary').textContent);
`)
}

// 实时任务卡只在进入页面时启动一次：reattachQueueView 会被 5s 全局轮询（refreshVisible）
// 反复调用，启动若挂在它里面，用户选的刷新间隔与「关」就形同虚设。
func TestAppJSLiveTasksStartsOnlyOnEntry(t *testing.T) {
	runLiveNode(t, "live-start", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.strictEqual(liveCalls(), 1, '进入本页拉一次');
  assert.strictEqual(timers.size, 1);
  const timerIds = [...timers.keys()].join();
  for (let i = 0; i < 12; i++) { vm.runInContext('reattachQueueView()', ctx); await flush(); }
  assert.strictEqual(liveCalls(), 1, '全局轮询不该驱动实时任务');
  assert.strictEqual([...timers.keys()].join(), timerIds, '全局轮询不该重置定时器');

  // 「关」：定时器撤掉，再进页面只拉一次、不再起定时器
  nodes.get('liveEvery').onchange({ target: { value: '0' } });
  assert.strictEqual(timers.size, 0);
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.strictEqual(timers.size, 0, '关闭后进入本页不该起定时器');
  assert.strictEqual(liveCalls(), 2);
`)
}

// 数据没变就不重画：账号多时一轮是几百个分组，每个间隔重建一遍 DOM 纯属浪费，还会
// 冲掉用户正在选中的文字。失败提示在恢复后要收起——包括「数据没变」直接早退的路径。
func TestAppJSLiveTasksSkipsUnchangedRedraw(t *testing.T) {
	runLiveNode(t, "live-redraw", `
  liveReply = {age_sec: 0, accounts: [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const writes = innerWrites;
  assert(writes > 0);
  liveReply = {age_sec: 30, accounts: liveReply.accounts};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes, '数据没变不该重画');
  assert(nodes.get('liveSummary').textContent.includes('30 秒前'), nodes.get('liveSummary').textContent);

  apiFail = true;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(nodes.get('liveNote').hidden, false);
  assert(nodes.get('liveNote').textContent.includes('boom'), nodes.get('liveNote').textContent);
  apiFail = false;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes);
  assert.strictEqual(nodes.get('liveNote').hidden, true, '数据没变时恢复后提示也要收起');
`)
}

// 后端那一轮还没跑完时返回的是上一份快照：页面要几秒后自动再取，否则卡片总是比数据
// 慢一个轮询间隔（默认 2 分钟）。这只是读服务端缓存；一轮结束（refreshing=false）就停。
func TestAppJSLiveTasksFollowsUpWhileRefreshing(t *testing.T) {
	runLiveNode(t, "live-refreshing", `
  const rows = [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}];
  liveReply = {refreshing: true, accounts: rows};
  vm.runInContext("view = 'taskscenter'", ctx);
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 1, '还在刷新就该续约一次再取');
  const before = liveCalls();
  [...timeouts.values()][0].fn(); await flush();
  assert.strictEqual(liveCalls(), before + 1);

  liveReply = {refreshing: false, accounts: rows};
  timeouts.clear();
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 0, '一轮结束不再续约');
`)
}

// 后台标签页不轮询：切走就别把上游当靶子练；可见性监听只绑一次，且进页即可续上。
func TestAppJSLiveTasksSkipsHiddenTab(t *testing.T) {
	runLiveNode(t, "live-hidden", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  const before = liveCalls();
  doc.hidden = true;
  [...timers.values()][0].fn(); await flush();
  assert.strictEqual(liveCalls(), before, '后台标签页不该拉');

  doc.hidden = false;
  [...timers.values()][0].fn(); await flush();
  assert.strictEqual(liveCalls(), before + 1, '回到前台继续');

  // 可见性监听只绑一次（多次进页不该叠加 listener）
  vm.runInContext("go('taskscenter')", ctx); await flush();
  vm.runInContext("go('accounts')", ctx); await flush();
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert.strictEqual(vis.length, 1, 'visibilitychange 只绑一次');
  assert.strictEqual(timers.size, 1, '进出本页不该叠出多个定时器');
  // 切到别的视图后，可见性回调不该拉数据
  vm.runInContext("go('accounts')", ctx); await flush();
  doc.hidden = false;
  const n = liveCalls();
  vis[0](); await flush();
  assert.strictEqual(liveCalls(), n, '不在本页时可见性回调不拉数据');
  // 回到本页后恢复
  vm.runInContext("view = 'taskscenter'", ctx);
  vis[0](); await flush();
  assert.strictEqual(liveCalls(), n + 1);
`)
}

// 「立即刷新」走 fresh=1：绕过后端 TTL 强制开一轮；普通轮询不带 fresh。
func TestAppJSLiveTasksFreshReload(t *testing.T) {
	runLiveNode(t, "live-fresh", `
  vm.runInContext("go('taskscenter')", ctx); await flush();
  assert(liveCalls() === 1 && calls.includes('tasks/live'), calls.join());
  nodes.get('btnLiveReload').onclick(); await flush();
  assert(calls.includes('tasks/live?fresh=1'), '立即刷新要带 fresh=1：' + calls.join());
`)
}

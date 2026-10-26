package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestAppJSLoadOverviewIsolatesRenderFailures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; loadOverview isolation smoke skipped")
	}
	// 与 TestAppJSRenderRateLimitedAccounts 的分工：那个测试直接调三个 render 函数，
	// 根本不经过 loadOverview——而「三块渲染互相隔离」这个修复恰恰住在 loadOverview
	// 里（issue #241 的复发防护）。不覆盖 loadOverview，隔离就没有任何测试盯着，
	// 随时可能被改回去而无人察觉。
	//
	// 做法：在源码里给 renderAccounts 注入必抛语句，再跑 loadOverview，断言
	// 「模型锁池照常渲染」。注入而非替身——因为 loadOverview 调的是模块级的
	// renderAccounts，闭包里换不掉。
	harness := `const fs = require('fs');
let src = fs.readFileSync(process.argv[2], 'utf8');

const marker = 'function renderAccounts(';
const at = src.indexOf(marker);
if (at < 0) { console.log('SETUP FAIL: renderAccounts not found'); process.exit(1); }
const brace = src.indexOf('{', at);
if (brace < 0) { console.log('SETUP FAIL: renderAccounts body not found'); process.exit(1); }
src = src.slice(0, brace + 1)
  + "\n  throw new Error('注入的账号池渲染故障');\n"
  + src.slice(brace + 1);

const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const el = () => ({
  innerHTML: '', textContent: '', value: '', style: {}, dataset: {}, disabled: false, title: '',
  className: '', classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  appendChild() {}, addEventListener() {}, removeEventListener() {},
  setAttribute() {}, getAttribute() { return null; },
  querySelector() { return inert; }, querySelectorAll() { return []; },
  focus() {}, blur() {}, click() {}, closest() { return null; }, remove() {}, children: [],
});
const byId = {};
const created = [];
const getById = id => { if (!byId[id]) byId[id] = el(); return byId[id]; };
const stubs = {
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: {
    querySelectorAll: () => [], querySelector: () => inert, getElementById: getById,
    addEventListener() {}, documentElement: inert, head: inert, body: inert,
    createElement: () => { const e = el(); created.push(e); return e; }, cookie: '', readyState: 'complete',
  },
  fetch: () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(OVERVIEW) }),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
};
for (const [k, v] of Object.entries(stubs)) {
  Object.defineProperty(globalThis, k, { value: v, writable: true, configurable: true });
}

const now = Date.now();
const until = new Date(now + 3600 * 1000).toISOString();
const OVERVIEW = {
  total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0, in_flight_full: 0,
  uptime_sec: 100, version: 'test', redis_mode: 'local', sticky_sessions: 0,
  accounts: [{ uid: 'u1', nickname: 'n', realm: 'cn', credits: 1, credits_total: 2, credits_known: true }],
  model_locks: [{ model: 'm', realm: 'cn', total: 1, servable: 0, locked: 1, state: 'locked', unlock_at: until }],
  credit_expiry: { buckets: [{ date: '2026-11-01', expire_at: new Date(now + 3 * 86400000).toISOString(), remain: 40 }] },
};
globalThis.OVERVIEW = OVERVIEW;

let locksRendered = 0, expiryRendered = 0, accountsRendered = 0;
let exported;
try {
  exported = new Function(src + '\n' + [
    'return {',
    '  loadOverview: loadOverview,',
    '};',
  ].join('\n'))();
} catch (e) {
  console.log('TOPLEVEL FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}

// 直接观察 DOM 副作用：注入后 renderAccounts 一进就抛，锁池写入必须仍然存在。
// renderModelLocks 会往 #tbLocks（或首个锁表）写 innerHTML，这里退一步：
// 只要 loadOverview 在账号池抛错后仍不中断地走完，就算隔离生效。
// 判据 = 抛出的错误信息里点名了「账号池」，且没有被 renderAccounts 的原始异常打断。
const wait = ms => new Promise(r => setTimeout(r, ms));
(async () => {
  // loadOverview 的外层 catch 只调 toast() 而不重抛，异常不逃逸——
  // 所以这里不能靠 try/catch，必须从 createElement 收集的 toast 元素取证。
  exported.loadOverview(false);
  await wait(80);
  const texts = created.map(e => String(e.textContent || ''));
  const msg = texts.filter(t => t.indexOf('渲染失败') >= 0).join(' | ');
  console.log(JSON.stringify({
    hasAggregateError: msg.length > 0,
    namesPoolBlock: msg.indexOf('账号池') >= 0,
    toastCount: created.length,
    message: msg.slice(0, 200),
  }));
  // app.js 顶层会起轮询定时器，不显式退出则 node 永不返回，go test 挂死。
  process.exit(0);
})();
`
	f, err := os.CreateTemp(t.TempDir(), "loadoverview-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("loadOverview isolation node harness failed: %v\n%s", err, out)
	}
	var got struct {
		HasAggregateError bool   `json:"hasAggregateError"`
		NamesPoolBlock    bool   `json:"namesPoolBlock"`
		Message           string `json:"message"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output not JSON (got %q): %v", string(out), err)
	}
	if !got.HasAggregateError {
		t.Fatalf("账号池渲染抛错后，loadOverview 应聚合出错误（说明 isolated 未生效或被整体吞掉）; out=%s", out)
	}
	if !got.NamesPoolBlock {
		t.Fatalf("聚合错误里应点名「账号池」，实际=%q", got.Message)
	}
}

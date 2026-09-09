// 报告 JS 校验：无残留占位符、grid 无重复声明、图表代码可执行
// canvas stub 自动发现：从 HTML 里扫描 id="c_*" 的画布，全部注册为全局空对象
// 模板尾部现在有视图切换器 script（跑在图表配置之后）——eval 全部内联脚本，
// DOM 桩提供最小面（querySelectorAll 返回空、元素桩带 style/addEventListener）让切换器安全空跑。
const fs = require('fs');
const files = process.argv.slice(2);
let fail = 0;
function stubEl() {
  return { style: {}, addEventListener: () => {}, getAttribute: () => null,
           textContent: '', rows: [], cells: [], nextElementSibling: null,
           tagName: 'DIV', className: '', parentNode: { style: {} } };
}
for (const f of files) {
  const html = fs.readFileSync(f, 'utf8');
  const leftovers = html.match(/__(TOKENS|RUN1|RUN2|THINK_SEC|MT_LABELS|MT_TTFT|MT_CTX|LEVELS|THR_OFF|THR_ON|TTFT_OFF|TTFT_ON|GRID|BODY1|BODY2|P50_OFF|P50_ON|P95_OFF|P95_ON)__/g);
  const gridDecls = (html.match(/const grid=/g) || []).length;
  let chartCalls = [];
  global.Chart = function (cv, cfg) { chartCalls.push(cfg.type); };
  global.Chart.instances = {};
  global.window = global;
  global.document = {
    getElementById: (id) => global[id] || (global[id] = stubEl()),
    querySelector: () => stubEl(),
    querySelectorAll: () => [],
  };
  for (const m of html.matchAll(/id="(c_[a-z0-9_]+)"/g)) {
    global[m[1]] = {};
  }
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
  try {
    eval(scripts[0]); // Chart.js 库
    // 真实 Chart.js 会覆盖计数桩且在桩 DOM 下无法创建 canvas context——
    // 重装计数桩：校验只关心"图表配置确实调用了 Chart"，不关心真渲染
    global.Chart = function (cv, cfg) { chartCalls.push(cfg.type); };
    global.Chart.instances = {};
    for (const s of scripts.slice(1)) {
      eval(s); // 图表配置 + 视图切换器（按模板顺序）
    }
    console.log(f, '| 残留占位符:', leftovers ? leftovers : '无', '| grid声明:', gridDecls, '| 图表:', chartCalls.join(',') || '无');
    if (leftovers || chartCalls.length === 0) fail = 1;
  } catch (e) {
    console.log(f, '| 执行失败:', e.message);
    fail = 1;
  }
}
process.exit(fail);

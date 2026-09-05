// 报告 JS 校验：无残留占位符、grid 无重复声明、图表代码可执行
const fs = require('fs');
const files = process.argv.slice(2);
let fail = 0;
for (const f of files) {
  const html = fs.readFileSync(f, 'utf8');
  const leftovers = html.match(/__(TOKENS|RUN1|RUN2|THINK_SEC|MT_LABELS|MT_TTFT|MT_CTX|LEVELS|THR_OFF|THR_ON|TTFT_OFF|TTFT_ON|GRID)__/g);
  const gridDecls = (html.match(/const grid=/g) || []).length;
  let chartCalls = [];
  global.Chart = function (cv, cfg) { chartCalls.push(cfg.type); };
  global.c_multiturn = {}; global.c_cc1 = {}; global.c_cc2 = {}; global.c_s1 = {}; global.c_s2 = {};
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
  try {
    eval(scripts[0]); // Chart.js 库
    eval(scripts[scripts.length - 1]); // 图表配置
    console.log(f, '| 残留占位符:', leftovers ? leftovers : '无', '| grid声明:', gridDecls, '| 图表:', chartCalls.join(',') || '无');
    if (leftovers || chartCalls.length === 0) fail = 1;
  } catch (e) {
    console.log(f, '| 执行失败:', e.message);
    fail = 1;
  }
}
process.exit(fail);

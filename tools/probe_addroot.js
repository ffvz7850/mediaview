// 真实交互探针：用 jsdom 加载真实 index.html + app.js，模拟「点击添加文件夹」。
//
// 用途：这条链路（按钮 → 面板可见 → 选目录 → 进列表 → 保存）**只在浏览器里成立**，
//   纯 Go 的静态断言只能证明"DOM 位置对"，证明不了"点下去真的有动作"。
//   v2.4.17 的 bug（面板嵌在 #thumbOpts 里，关掉缩略图后点了毫无反应）就是这类问题。
//
// 需要一次性安装依赖：
//   cd tools && npm i jsdom
// 运行：
//   node tools/probe_addroot.js            # 默认读 ../src
//   node tools/probe_addroot.js <src目录>
//
// 改动这块 UI 时跑一次；四个变异体（面板搬回 thumbOpts / 去掉 scrollIntoView /
// 无视用途 toggle / dbPick 不分流）都应让它变红。
const fs = require('fs');
const path = require('path');
const { JSDOM, VirtualConsole } = require('jsdom');

const SRC = process.argv[2] || path.join(__dirname, '..', 'src');
const htmlPath = path.join(SRC, 'web/index.html');
const jsPath = path.join(SRC, 'web/app.js');

let html = fs.readFileSync(htmlPath, 'utf8');
// 去掉外链脚本，手动 eval（便于控制时序与桩）
html = html.replace(/<script[^>]*src=[^>]*><\/script>/g, '');

const errors = [];
const vc = new VirtualConsole();
vc.on('jsdomError', (e) => errors.push('jsdomError: ' + (e && e.message)));
vc.on('error', (...a) => errors.push('console.error: ' + a.join(' ')));

const dom = new JSDOM(html, {
  runScripts: 'outside-only',
  pretendToBeVisual: true,
  url: 'http://127.0.0.1/app/mediaview/',
  virtualConsole: vc,
});
const { window } = dom;

// ---- 环境桩 ----
window.matchMedia = window.matchMedia || function () {
  return { matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} };
};
if (!window.ResizeObserver) window.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
if (!window.IntersectionObserver) window.IntersectionObserver = class { observe() {} unobserve() {} disconnect() {} takeRecords() { return []; } };
// jsdom 没有 scrollIntoView（我们的代码用 try/catch 包着，这里仍然补上以便断言被调用）
const scrollCalls = [];
window.Element.prototype.scrollIntoView = function () { scrollCalls.push(this.id || this.className); };

const fetchCalls = [];
function jsonResp(obj, code) {
  return Promise.resolve({
    ok: (code || 200) < 300,
    status: code || 200,
    json: () => Promise.resolve(obj),
    text: () => Promise.resolve(JSON.stringify(obj)),
  });
}
const fetchPosts = [];
window.fetch = function (url, opt) {
  fetchCalls.push(String(url));
  if (opt && opt.body) {
    try { fetchPosts.push({ url: String(url), body: JSON.parse(opt.body) }); } catch (e) {}
  }
  const u = String(url);
  if (u.includes('/api/settings')) {
    return jsonResp({
      settings: {
        thumbEnabled: true, coverEnabled: true, thumbSize: 320, thumbQuality: 80,
        onlyRoots: false, roots: [], rootsInfo: [],
        theme: 'dark', viewerMode: 'window', activeThumbDir: '/tmp/thumbs',
        cacheFiles: 0, cacheBytes: 0, defaultThumbDir: '/tmp/thumbs',
      },
    });
  }
  if (u.includes('/api/list')) {
    // 模拟 2.4.18 的展开结果：首页 = 已添加文件夹里的子文件夹 + 散图
    return jsonResp({
      path: '/', parent: '', sort: 'name',
      dirs: [
        { name: '2023', path: '/vol1/1000/照片库/2023' },
        { name: '2024', path: '/vol1/1000/照片库/2024' },
      ],
      files: [
        { name: '散图.jpg', path: '/vol1/1000/照片库/散图.jpg', kind: 'image', size: 1, mtime: 1, ext: '.jpg' },
      ],
      total: 3,
    });
  }
  if (u.includes('/api/browse')) {
    return jsonResp({
      path: '/', parent: '', self: '/tmp/thumbs',
      dirs: [{ name: 'vol1', path: '/vol1' }, { name: 'pics', path: '/vol1/1000/pics' }],
    });
  }
  return jsonResp({});
};

const appjs = fs.readFileSync(jsPath, 'utf8');
const runtimeErrors = [];
try {
  window.eval(appjs);
} catch (e) {
  runtimeErrors.push('eval: ' + e.message);
}

// boot() 里的 promise 链：让它跑完
const tick = () => new Promise((r) => setTimeout(r, 30));

function ok(cond, msg) {
  if (!cond) {
    console.error('FAIL ' + msg);
    process.exitCode = 1;
  }
}

(async () => {
  await tick();
  await tick();

  const doc = window.document;
  const $ = (id) => doc.getElementById(id);

  ok(!!$('btnAddRoot'), '按钮 #btnAddRoot 存在');
  ok(!!$('dirBrowser'), '面板 #dirBrowser 存在');
  ok(!!$('dbPick'), '#dbPick 存在');

  // ===== 2.4.18：首页必须把「已添加的文件夹」展开成内容层 =====
  //
  // 用户诉求：「软件内应该显示文件夹内的子文件夹，而不是单个文件夹当第一层级」。
  // 后端负责展开，前端负责把它渲染出来 —— 这里验的是渲染这一半：
  // ① 子文件夹出现在首页；② 根目录的散图也在（否则因为 root 已不是格子，那些图永远没入口）。
  const cells = doc.querySelectorAll('.cell');
  ok(cells.length === 3, '★ 首页应渲染 3 个格子（2 个子文件夹 + 1 张散图），实际 ' + cells.length);
  ok(doc.querySelectorAll('.cell .cell-cover').length >= 2,
    '★★ 两个子文件夹必须作为文件夹格子渲染（有 .cell-cover 封面结构），实际 ' +
    doc.querySelectorAll('.cell .cell-cover').length);
  ok(doc.querySelectorAll('.cell img').length >= 0, '散图格子不应抛异常');

  // ★ 核心：面板不能在 #thumbOpts 里
  const thumbOpts = $('thumbOpts');
  ok(thumbOpts && !thumbOpts.contains($('dirBrowser')),
    '★★ #dirBrowser 不在 #thumbOpts 内 —— 否则关闭“生成缩略图”时整块 display:none，点击必然无反应');

  // 初始应隐藏
  ok($('dirBrowser').classList.contains('hidden'), '初始状态面板隐藏');

  // ① 模拟点击“添加文件夹”
  $('btnAddRoot').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();

  ok(!$('dirBrowser').classList.contains('hidden'), '★ 点「添加文件夹」后面板必须显示（这是本次 bug 的直接验收点）');

  // ★★ 就地展开：面板必须落在「只显示我添加的文件夹」这块里，紧接按钮之后。
  //    固定在设置主体末尾时，它正好贴在「缩略图缓存」（清空缓存/清除所有数据）下面，
  //    用户看到的就是"跳到下面的缩略图缓存位置"。
  const box = $('dirBrowser');
  ok(box.parentNode && box.parentNode.id === 'rootBrowserHost',
    '★★ 面板必须移入 #rootBrowserHost（就地展开），实际父节点 = ' +
    (box.parentNode ? (box.parentNode.id || box.parentNode.className) : 'null'));
  const rootBlock = $('btnAddRoot').closest('.set-block');
  ok(rootBlock && rootBlock.contains(box),
    '★★ 面板必须和「添加文件夹」按钮在同一个区块内（否则用户会以为跳到了缩略图区域）');
  // DOM 顺序：按钮在面板之前
  ok(rootBlock && (rootBlock.compareDocumentPosition(box) & window.Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
    '面板应出现在按钮之后（就地往下展开）');
  // 反向断言：绝不能贴在「缩略图缓存」那块里或紧邻其后
  const cacheBlock = $('cacheInfo') ? $('cacheInfo').closest('.set-block') : null;
  ok(!(cacheBlock && cacheBlock.contains(box)),
    '★★ 面板不得落在「缩略图缓存」区块内 —— 那正是用户报的"跳到缩略图缓存位置"');
  ok($('thumbBrowserHost') !== box.parentNode,
    '此时面板不该在缩略图宿主里');
  ok($('dbPick').textContent === '添加此目录', '★ 按钮文案应切换为「添加此目录」，实际 ' + JSON.stringify($('dbPick').textContent));
  ok(fetchCalls.some((u) => u.includes('/api/browse')), '应请求 /api/browse 取得目录列表');
  ok(scrollCalls.includes('dirBrowser'), '应把面板滚到可见（否则用户看不到 = 像没反应）');

  // 目录列表渲染
  const items = doc.querySelectorAll('#dbList .db-item');
  ok(items.length === 2, '目录列表应渲染 2 项，实际 ' + items.length);

  // ② 进入子目录 → 用此目录
  items[1].dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  $('dbPick').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();

  const listBox = $('rootList');
  ok(listBox && listBox.querySelectorAll('.root-item').length === 1,
    '★ 点「添加此目录」后列表应多一项，实际 ' + (listBox ? listBox.querySelectorAll('.root-item').length : 'null'));
  ok($('dirBrowser').classList.contains('hidden'), '选完后面板应收起');
  ok(/已添加/.test($('rootHint').textContent || ''), '应给出「已添加」提示，实际 ' + JSON.stringify($('rootHint').textContent));

  // ③ 再点一次应展开；再点第三次应收起（toggle）
  $('btnAddRoot').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(!$('dirBrowser').classList.contains('hidden'), '再次点「添加文件夹」应展开');
  $('btnAddRoot').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok($('dirBrowser').classList.contains('hidden'), '★ 第三次点击应收起（切换手感）');

  // ④ 「选择目录」与共用面板的用途切换
  $('btnBrowse').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(!$('dirBrowser').classList.contains('hidden'), '点「选择目录」应展开同一面板');
  ok($('dbPick').textContent === '用此目录', '用途切到缩略图目录后文案应回到「用此目录」，实际 ' + JSON.stringify($('dbPick').textContent));
  ok(box.parentNode && box.parentNode.id === 'thumbBrowserHost',
    '★ 点「选择目录」时面板应移回 #thumbBrowserHost，实际 ' +
    (box.parentNode ? (box.parentNode.id || box.parentNode.className) : 'null'));

  // 面板开着时点「添加文件夹」→ 应切换用途而不是关闭
  $('btnAddRoot').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(!$('dirBrowser').classList.contains('hidden'), '★ 面板开着时应切换用途，不能直接关掉');
  ok($('dbPick').textContent === '添加此目录', '切换后文案应为「添加此目录」');

  // ⑤ 关闭按钮
  $('dbClose').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok($('dirBrowser').classList.contains('hidden'), '✕ 关闭按钮应生效');

  // ⑥ 关键回归：关掉「生成缩略图」后，添加文件夹仍要能打开
  $('setThumbEnabled').checked = false;
  $('setThumbEnabled').dispatchEvent(new window.Event('change', { bubbles: true }));
  await tick();
  ok($('thumbOpts').style.display === 'none', '关闭缩略图后 #thumbOpts 应隐藏（复现用户环境）');
  $('btnAddRoot').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(!$('dirBrowser').classList.contains('hidden'),
    '★★ 在 #thumbOpts 隐藏的情况下点「添加文件夹」仍必须能打开面板（原 bug 就是这里失效）');

  // ⑦ 保存：请求体必须带上 roots / onlyRoots
  //   前面六步只到"列表里有一项"，但用户要的是**添加生效** ——
  //   pendingRoots 忘了塞进 saveSettings 的 body，界面看着加上了、保存后照样是空的。
  const pendingCount = $('rootList').querySelectorAll('.root-item').length;
  ok(pendingCount >= 1, '保存前列表应有条目，实际 ' + pendingCount);
  $('settingsSave').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  await tick();
  await tick();
  const saved = fetchPosts.filter((x) => x.url.includes('/api/settings')).pop();
  ok(!!saved, '★ 点「保存」必须发出 POST /api/settings');
  if (saved) {
    ok(Array.isArray(saved.body.roots) && saved.body.roots.length >= 1,
      '★★ 保存请求体必须带上 roots（否则列表里加了、保存后还是空）实际 ' + JSON.stringify(saved.body.roots));
    ok(saved.body.onlyRoots === true || saved.body.onlyRoots === false,
      '保存请求体必须带 onlyRoots（布尔），实际 ' + JSON.stringify(saved.body.onlyRoots));
  }

  if (runtimeErrors.length) {
    runtimeErrors.forEach((e) => console.error('FAIL 脚本异常 ' + e));
    process.exitCode = 1;
  }
  if (process.exitCode) {
    if (errors.length) console.error('页面错误:\n  ' + errors.slice(0, 8).join('\n  '));
    console.log('PROBE FAILED');
  } else {
    console.log('PROBE OK  面板可见/文案切换/列表新增/toggle/共用面板/关闭按钮/缩略图关闭场景 全部通过');
  }
  dom.window.close();
})();

// 2.4.20 真实交互探针：封面名称「半透明底 + 灰度自适应字色」，以及两种名称位置。
//
// 为什么需要它：Go 里只能断言"源码里写了这些字符串 / CSS 里有这条规则"，
// 证明不了「图片加载完成的那一刻，格子真的按灰度换上了黑白字」。
// 这里用 jsdom 真跑页面，并把 canvas 换成一个"假 2d 上下文"（jsdom 没实现 canvas，
// 装 native 版代价太大），于是整条链路可以离线跑通：
//   img.onload → sampleCoverTone(画底部 30% → 取平均灰度) → pickCoverTone → 加 class
//
// 覆盖：
//   A. 默认（coverNameBelow 关）⇒ 名称压在封面底部，且 4 个格子按灰度分别拿到 dark/bright
//   B. 开（coverNameBelow 开）⇒ 回到"封面下方独立一行"，封面内不再叠字
//
// 需要一次性安装依赖： cd tools && npm i jsdom
// 运行： node tools/probe_covername.js [src目录]
const fs = require('fs');
const path = require('path');
const { JSDOM, VirtualConsole } = require('jsdom');

const SRC = process.argv[2] || path.join(__dirname, '..', 'src');
const html = fs
  .readFileSync(path.join(SRC, 'web/index.html'), 'utf8')
  .replace(/<script[^>]*src=[^>]*><\/script>/g, '');
const appjs = fs.readFileSync(path.join(SRC, 'web/app.js'), 'utf8');

// 四档灰度，落在阈值 190 的两侧（189/190 是边界）
const DIRS = [
  { name: '暗图', path: '/vol1/1000/照片库/dark', mean: 30, tone: 'dark' },
  { name: '临界暗', path: '/vol1/1000/照片库/edge-dark', mean: 189, tone: 'dark' },
  { name: '临界亮', path: '/vol1/1000/照片库/edge-bright', mean: 190, tone: 'bright' },
  { name: '亮图', path: '/vol1/1000/照片库/bright', mean: 250, tone: 'bright' },
];

const tick = () => new Promise((r) => setTimeout(r, 30));

let failed = 0;
function ok(cond, msg) {
  if (!cond) {
    console.error('FAIL ' + msg);
    failed++;
  }
}

async function boot(settings) {
  const vc = new VirtualConsole();
  const pageErrors = [];
  vc.on('jsdomError', (e) => pageErrors.push('jsdomError: ' + (e && e.message)));

  const dom = new JSDOM(html, {
    runScripts: 'outside-only',
    pretendToBeVisual: true,
    url: 'http://127.0.0.1/app/mediaview/?dir=%2Fvol1%2F1000%2F%E7%85%A7%E7%89%87%E5%BA%93',
    virtualConsole: vc,
  });
  const { window } = dom;
  window.matchMedia = window.matchMedia || function () {
    return { matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} };
  };
  if (!window.ResizeObserver) window.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
  window.Element.prototype.scrollIntoView = function () {};
  // jsdom 不做布局，clientWidth/Height 恒为 0 —— 网格会按"可见 0 行"只渲染前两行，
  // 第 5 个格子（散图）根本不会创建。补一组固定尺寸，让虚拟滚动按真实屏幕算。
  for (const prop of ['clientWidth', 'clientHeight', 'offsetWidth', 'offsetHeight']) {
    Object.defineProperty(window.HTMLElement.prototype, prop, {
      configurable: true,
      get() { return prop.indexOf('Height') >= 0 ? 800 : 1200; },
    });
  }

  // ---- 假 canvas：jsdom 没有 2d 上下文，这里按 img.__mean 造像素 ----
  // 只实现 sampleCoverTone 用到的 drawImage / getImageData 两个方法。
  const fakeCtx = {
    _img: null,
    drawImage(img) { this._img = img; },
    getImageData(x, y, w, h) {
      const mean = (this._img && this._img.__mean) || 0;
      const data = new Uint8ClampedArray(w * h * 4);
      for (let i = 0; i < data.length; i += 4) {
        data[i] = data[i + 1] = data[i + 2] = mean;
        data[i + 3] = 255;
      }
      return { data };
    },
  };
  window.HTMLCanvasElement.prototype.getContext = function () { return fakeCtx; };

  const posts = [];
  const jsonResp = (obj) => Promise.resolve({
    ok: true, status: 200,
    json: () => Promise.resolve(obj),
    text: () => Promise.resolve(JSON.stringify(obj)),
  });
  window.fetch = function (url, opt) {
    const isPost = !!(opt && opt.method === 'POST');
    if (isPost && opt.body) {
      try { posts.push({ url: String(url), body: JSON.parse(opt.body) }); } catch (e) {}
    }
    const u = String(url);
    // ⚠️ 两个接口的返回形状不一样：GET 是 Settings 平铺在顶层，POST 才是 {ok,settings,note}
    if (u.includes('/api/settings')) {
      return jsonResp(isPost ? { ok: true, settings: settings, note: '' } : settings);
    }
    if (u.includes('/api/list')) {
      return jsonResp({
        path: '/vol1/1000/照片库', parent: '/vol1/1000', sort: 'name',
        dirs: DIRS.map((d) => ({ name: d.name, path: d.path })),
        files: [{ name: '散图.jpg', path: '/vol1/1000/照片库/散图.jpg', kind: 'image', size: 1, mtime: 1, ext: '.jpg' }],
        total: DIRS.length + 1,
      });
    }
    return jsonResp({});
  };

  try { window.eval(appjs); } catch (e) { pageErrors.push('eval: ' + e.message); }
  await tick(); await tick();
  return { dom, doc: window.document, window, posts, pageErrors };
}

// loadCover 模拟"封面图下载完成"：jsdom 不会真的取图，所以手动把 src 换成非占位图、
// 补上 naturalWidth/Height（否则取样函数会因为尺寸为 0 直接返回空），再派发 load。
function loadCover(window, img, mean) {
  img.src = 'http://127.0.0.1/app/mediaview/api/dircov?probe=1';
  Object.defineProperty(img, 'naturalWidth', { value: 320, configurable: true });
  Object.defineProperty(img, 'naturalHeight', { value: 320, configurable: true });
  img.__mean = mean;
  img.dispatchEvent(new window.Event('load'));
}

const baseSettings = {
  thumbEnabled: true, coverEnabled: true, thumbSize: 320, thumbQuality: 80,
  onlyRoots: false, roots: [], rootsInfo: [],
  viewerMode: 'window', activeThumbDir: '/tmp/thumbs', cacheFiles: 0, cacheBytes: 0,
  defaultThumbDir: '/tmp/thumbs',
};

(async () => {
  // ================= A. 默认：压在封面底部 + 灰度自适应 =================
  const A = await boot(Object.assign({}, baseSettings)); // 不带 coverNameBelow ⇒ 默认关
  const docA = A.doc;

  const innersA = Array.from(docA.querySelectorAll('.cell-inner')).filter((el) => el.querySelector('.cell-cover'));
  ok(innersA.length === DIRS.length, `应有 ${DIRS.length} 个文件夹格子，实际 ${innersA.length}`);

  innersA.forEach((inner, i) => {
    ok(!inner.classList.contains('mv-name-below'),
      `★ 默认不该带 mv-name-below（默认是压在封面底部）第 ${i + 1} 个格子却带了`);
    ok(!inner.querySelector('.cell-name-below'),
      `★ 默认不该有 .cell-name-below（那是"独立一行"模式）第 ${i + 1} 个格子却有了`);
    const cover = inner.querySelector('.cell-cover');
    ok(!!cover && !!cover.querySelector('.cell-label'),
      `★ 默认模式下名称必须在封面内（.cell-label）第 ${i + 1} 个格子没有`);
    ok(!!cover.querySelector('.cell-label-name'), '封面名称里要有一个 .cell-label-name 承载文字');
  });

  // 名称文字 = 各自的文件夹名
  const labels = innersA.map((i) => (i.querySelector('.cell-label-name') || {}).textContent || '');
  ok(labels.join('|') === DIRS.map((d) => d.name).join('|'),
    '★ 封面上的名字必须是各自的文件夹名，实际 ' + JSON.stringify(labels));

  // 灰度自适应：逐个"加载完成"，看拿到哪套配色
  innersA.forEach((inner, i) => {
    const cover = inner.querySelector('.cell-cover');
    const img = cover.querySelector('img.cell-cover-img');
    ok(!!img, `第 ${i + 1} 个格子应有封面图元素`);
    if (!img) return;
    ok(!cover.classList.contains('mv-has-cover'), '图还没加载时不该有 mv-has-cover（名称先不显示）');
    loadCover(A.window, img, DIRS[i].mean);
    ok(cover.classList.contains('mv-has-cover'), `★ 图加载完成后第 ${i + 1} 个格子应加 mv-has-cover`);
    const want = DIRS[i].tone;
    ok(cover.classList.contains('mv-tone-' + want),
      `★★ 灰度 ${DIRS[i].mean} 应判为 ${want}（第 ${i + 1} 个格子实际 class = ${cover.className}）`);
    ok(!cover.classList.contains('mv-tone-' + (want === 'bright' ? 'dark' : 'bright')),
      `★★ 两套配色不能同时存在（第 ${i + 1} 个格子）`);
  });

  // 图片格子不该有名称
  const imgInner = Array.from(docA.querySelectorAll('.cell-inner')).find((el) => !el.querySelector('.cell-cover'));
  ok(!!imgInner, '应有 1 个图片格子');
  if (imgInner) ok(!imgInner.querySelector('.cell-label'), '★ 图片格子不该有封面名称');

  if (A.pageErrors.length) A.pageErrors.forEach((e) => ok(false, '页面脚本异常 ' + e));

  // ================= B. 开着：封面下方独立一行 =================
  const B = await boot(Object.assign({}, baseSettings, { coverNameBelow: true }));
  const docB = B.doc;
  const innersB = Array.from(docB.querySelectorAll('.cell-inner')).filter((el) => el.querySelector('.cell-cover'));
  ok(innersB.length === DIRS.length, `模式B：应有 ${DIRS.length} 个文件夹格子，实际 ${innersB.length}`);
  innersB.forEach((inner, i) => {
    const nm = inner.querySelector('.cell-name-below');
    const cover = inner.querySelector('.cell-cover');
    ok(inner.classList.contains('mv-name-below'), `★ 模式B 第 ${i + 1} 个格子应带 mv-name-below`);
    ok(!!nm, `★ 模式B 第 ${i + 1} 个格子应有 .cell-name-below`);
    if (!nm || !cover) return;
    ok(nm.parentNode === inner, '★★ 名称行的父节点必须是 .cell-inner（封面之外）');
    ok(!cover.contains(nm), '★★ 名称行不能出现在 .cell-cover 内部');
    ok(nm.textContent === DIRS[i].name, `模式B 名称应为 ${DIRS[i].name}，实际 ${nm.textContent}`);
  });
  // 复选框由 openSettings() 的 fetch 回调回填，必须先真的打开面板
  docB.getElementById('btnSettings').dispatchEvent(new B.window.MouseEvent('click', { bubbles: true }));
  await tick(); await tick();
  ok(docB.getElementById('setCoverName').checked === true, '模式B：设置面板开关应回填为勾选');
  if (B.pageErrors.length) B.pageErrors.forEach((e) => ok(false, '模式B 页面脚本异常 ' + e));

  // 保存请求体必须带字段
  docB.getElementById('settingsSave').dispatchEvent(new B.window.MouseEvent('click', { bubbles: true }));
  await tick(); await tick();
  const saved = B.posts.filter((p) => p.url.includes('/api/settings')).pop();
  ok(!!saved, '点保存应发出 POST /api/settings');
  if (saved) ok(saved.body.coverNameBelow === true, '保存请求体里 coverNameBelow 应为 true');

  // 默认面板回填（模式A的实例）
  docA.getElementById('btnSettings').dispatchEvent(new A.window.MouseEvent('click', { bubbles: true }));
  await tick(); await tick();
  ok(docA.getElementById('setCoverName').checked === false, '模式A：设置面板开关默认未勾选');

  // 小视图档位：点「小视图」按钮必须给 body 挂 mv-small（CSS 靠它把名称字号收到 13px），
  // 切回大视图必须摘掉 —— 只有两条路径都通，才不会出现"小视图里名称仍然是 15px"。
  docA.getElementById('sizeSmall').dispatchEvent(new A.window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(A.dom.window.document.body.classList.contains('mv-small'), '★ 切到小视图后 body 应带 mv-small（否则名称条在 96px 格子里占近四成）');
  docA.getElementById('sizeLarge').dispatchEvent(new A.window.MouseEvent('click', { bubbles: true }));
  await tick();
  ok(!A.dom.window.document.body.classList.contains('mv-small'), '★ 切回大视图后 body 的 mv-small 必须摘掉');

  A.dom.window.close();
  B.dom.window.close();

  if (failed) {
    console.log('PROBE FAILED  共 ' + failed + ' 条不通过');
    process.exitCode = 1;
  } else {
    console.log('PROBE OK  默认压在封面底部 / 四档灰度(30·189·190·250)拿到 dark·dark·bright·bright / 名称只在文件夹格子 / 独立一行模式 / 保存请求体 全部通过');
  }
})();

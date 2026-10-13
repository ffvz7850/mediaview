/* fnos_sort.js —— 读取飞牛文件管理器的「目录排序偏好」，供媒体浏览跟随。
 *
 * 背景：飞牛文件管理器打开外部应用时只传 {path}，不传排序。媒体浏览想要
 * 「大图左右切换 / 大图预加载 / 缩略图预生成」与文件管理器里看到的顺序一致，
 * 只能自己去读文件管理器落盘的排序偏好。
 *
 * 文件管理器把普通视图的排序存在 IndexedDB：
 *   db = 'trim-file-manager' (version 1)
 *   store = 'file-view-preferences'
 *   记录 = { userId, scope:'my-files', surface:'directory',
 *            location:<去掉首尾斜杠的目录路径>,
 *            sorting:{ sortField, sortType } }
 *   sortField ∈ { name, mtim, size, type, btim }
 *   sortType ∈ { ASC, DESC }（飞牛写的是**大写**；1.8.183 起解析时统一小写比较）
 * 注意：当 sorting 与默认值（name asc）相同时该字段会被整段删除 —— 读不到就按
 * 默认 name asc 处理，而不是报错。
 *
 * 2.4.12：**只采用目录级记录**（location 与该目录匹配），不再采用全局默认
 * （location 为空）—— 全局默认是历史残留的列排序，与用户当前视图经常不符，
 * 采用它必然出现"点第 1 张显示 100/100"。没有目录级记录时由调用方兜底
 * name:asc（飞牛默认视图）。
 *
 * 本文件不依赖任何框架，纯函数（parse/normDir）可独立测试；getDirSort 是薄
 * 异步包装，任何异常都 resolve(null)，绝不让「跟随排序」这个增强功能把主流程搞挂。
 */
(function () {
  'use strict';

  var DB_NAME = 'trim-file-manager';
  var STORE = 'file-view-preferences';

  // 飞牛列头字段名 → 媒体浏览后端 parseSort 认的字段名。
  var FIELD_MAP = {
    name: 'name',
    mtim: 'mtime',
    size: 'size',
    type: 'type',
    btim: 'btime'
  };

  // 归一化目录路径：反斜杠转正斜杠、URL 解码、合并重复斜杠、去掉首尾斜杠。
  // file-view-preferences 的 location 存的就是「去掉首尾斜杠」的目录路径。
  //
  // 2.4.12：**必须 URL 解码**。看图窗口 URL 的 path 参数是编码的
  // （%2Fvol2%2F1000%2F%E5%9B%BE%E5%BA%93...），而 IndexedDB 里的 location 是
  // 解码后的真实路径。旧实现不解码 ⇒ 中文目录（尤其带《》等字符的）永远匹配
  // 不上目录级记录 ⇒ 只能退到全局默认 —— 全局默认又与当前视图不符，正是
  // "点第 1 张显示 100/100"的成因之一。
  function normDir(p) {
    if (!p) return '';
    var s = String(p).replace(/\\/g, '/');
    try {
      if (s.indexOf('%') >= 0) s = decodeURIComponent(s);
    } catch (e) {}
    s = s.replace(/\/+/g, '/');
    while (s.length > 1 && s.charAt(0) === '/') s = s.slice(1);
    while (s.length > 1 && s.charAt(s.length - 1) === '/') s = s.slice(0, -1);
    return s;
  }

  // 纯函数：从一批 file-view-preferences 记录里解析出指定目录的排序。
  // records: 数组，元素形如 { location, scope, surface, sorting, expiresAt }
  // dir: 目录路径（任意格式，内部归一化）
  // 返回 'field:dir' 字符串（如 'mtim:desc'），或 null（没找到/无排序信息）。
  //
  // 两级优先（2.4.12 起**不再采用全局默认**）：
  //   ① 该目录自己、scope=my-files 的记录
  //   ② 该目录自己、其它 scope 的记录
  //   ③ null —— 由调用方兜底 name:asc（飞牛默认视图）
  //
  // 为什么去掉"全局默认"（location 为空的记录）：
  //   全局默认是用户很久以前在飞牛里设的**列排序**，与"当前视图"经常不符
  //   （用户临时切换列/方向时飞牛不写偏好，只落盘最后设置的列）。
  //   用户真实数据：全局默认 name:desc、当前视图 name:asc ⇒ 采用全局默认必然
  //   "点第 1 张显示 100/100"（第 1 张是 0001.jpg，却按 name:desc 排到 100/100）。
  //   目录级记录才是"用户在这个目录里明确设过的排序"，可信得多；没有目录级
  //   记录时，跟随飞牛默认视图（name:asc）比猜一个历史残留值更接近用户所见。
  function parseFnOsViewPreference(records, dir) {
    if (!records || !records.length) return null;
    var target = normDir(dir);
    var now = Date.now();
    var best = null;
    for (var i = 0; i < records.length; i++) {
      var r = records[i];
      if (!r) continue;
      // 过期记录忽略：文件管理器自己也会清理，这里读到过期值会把顺序带偏
      if (r.expiresAt && r.expiresAt < now) continue;
      // 只认目录视图；搜索/分享等其它 surface 的同路径记录不套用。
      if (r.surface && r.surface !== 'directory') continue;
      if (!r.sorting || !r.sorting.sortField) continue;
      // 1.8.183：字段名与方向都做**大小写不敏感**归一。
      //   实测飞牛写的是**大写** DESC / ASC（它前端常量 wr='DESC' / Cr='ASC'），
      //   而这里原来只比小写 'desc' —— 于是**方向永远被当成 asc**：
      //   表现就是"文件管理器里按名称降序，大图里却按升序切换"（顺序整个反了）。
      var fRaw = String(r.sorting.sortField || '').toLowerCase().trim();
      var f = FIELD_MAP[fRaw];
      if (!f) continue; // 飞牛新增了不认识字段 → 忽略，别硬套默认制造错序
      var stRaw = String(r.sorting.sortType == null ? '' : r.sorting.sortType).toLowerCase().trim();
      var d = stRaw === 'desc' ? 'desc' : 'asc';
      var out = f + ':' + d;
      // 2.4.12：只匹配目录级记录，不再收集全局默认
      if (normDir(r.location) !== target) continue;
      // scope 通常是 'my-files'，命中直接返回；其它 scope（若有）留作兜底。
      if ((r.scope || 'my-files') === 'my-files') return out;
      if (!best) best = out;
    }
    return best || null;
  }

  // 本次会话内缓存：同一目录不必反复读库；也保证「第一次读到的值」稳定可用。
  var dirSortCache = {};

  // 异步读取指定目录的排序，返回 Promise<string|null>。
  // 带一次重试：实测首次打开时 IndexedDB 连接刚建立会读不到（日志里表现为
  // usedSort 退回了本地残留值），延迟重试一次即可稳定命中。
  function getFnOsDirSort(dir, retry) {
    if (dirSortCache[dir] !== undefined) {
      return Promise.resolve(dirSortCache[dir]);
    }
    return readOnce(dir).then(function (v) {
      if (v) { dirSortCache[dir] = v; return v; }
      if (retry) {
        return new Promise(function (res) { setTimeout(res, 250); }).then(function () {
          return getFnOsDirSort(dir, false);
        });
      }
      return null;
    });
  }

  function readOnce(dir) {
    return new Promise(function (resolve) {
      var done = false;
      var finish = function (v) { if (!done) { done = true; resolve(v); } };
      try {
        if (!window.indexedDB) { finish(null); return; }
        var req = window.indexedDB.open(DB_NAME, 1);
        req.onerror = function () { finish(null); };
        req.onblocked = function () { finish(null); };
        req.onsuccess = function () {
          var db = req.result;
          var closeDb = function () { try { db.close(); } catch (e2) {} };
          try {
            if (!db.objectStoreNames.contains(STORE)) { closeDb(); finish(null); return; }
            var tx = db.transaction(STORE, 'readonly');
            // 2.0.0：用完即关。原来读到结果就 return、连接从不关闭 ——
            // 每读一个目录就留一个打开的 IndexedDB 连接，长会话跨很多目录会持续累积。
            tx.oncomplete = closeDb;
            tx.onabort = closeDb;
            tx.onerror = closeDb;
            var all = tx.objectStore(STORE).getAll();
            all.onsuccess = function () {
              finish(parseFnOsViewPreference(all.result, dir));
            };
            all.onerror = function () { finish(null); };
          } catch (e) { closeDb(); finish(null); }
        };
      } catch (e) { finish(null); }
    });
  }

  window.FnOsSort = {
    parse: parseFnOsViewPreference,
    getDirSort: getFnOsDirSort,
    normDir: normDir
  };
})();

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

  // 归一化目录路径：反斜杠转正斜杠、去掉首尾斜杠。file-view-preferences 的
  // location 存的就是「去掉首尾斜杠」的目录路径，比对前两边都必须归一。
  function normDir(p) {
    if (!p) return '';
    var s = String(p).replace(/\\/g, '/');
    while (s.length > 1 && s.charAt(0) === '/') s = s.slice(1);
    while (s.length > 1 && s.charAt(s.length - 1) === '/') s = s.slice(0, -1);
    return s;
  }

  // 纯函数：从一批 file-view-preferences 记录里解析出指定目录的排序。
  // records: 数组，元素形如 { location, scope, surface, sorting, expiresAt }
  // dir: 目录路径（任意格式，内部归一化）
  // 返回 'field:dir' 字符串（如 'mtim:desc'），或 null（没找到/无排序信息）。
  //
  // 三级优先：① 该目录自己、scope=my-files 的记录
  //           ② 该目录自己、其它 scope 的记录
  //           ③ **全局默认排序**（location 为空的记录）
  // ③ 是必须的：用户在文件管理器里没单独设过某目录时，排序来自全局默认；
  // 旧实现只精确匹配目录，于是"读不到"直接回退到本应用的 state.sort，
  // 表现就是「文件管理器里明明是修改时间升序，大图却按降序走」。
  function parseFnOsViewPreference(records, dir) {
    if (!records || !records.length) return null;
    var target = normDir(dir);
    var now = Date.now();
    var best = null;
    var globalSort = null;
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
      var loc = normDir(r.location);
      if (loc === '') {
        // 全局默认排序：留作最后兜底（不覆盖目录级记录）
        if (!globalSort) globalSort = out;
        continue;
      }
      if (loc !== target) continue;
      // scope 通常是 'my-files'，命中直接返回；其它 scope（若有）留作兜底。
      if ((r.scope || 'my-files') === 'my-files') return out;
      if (!best) best = out;
    }
    return best || globalSort || null;
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
          try {
            if (!db.objectStoreNames.contains(STORE)) { finish(null); return; }
            var tx = db.transaction(STORE, 'readonly');
            var all = tx.objectStore(STORE).getAll();
            all.onsuccess = function () {
              finish(parseFnOsViewPreference(all.result, dir));
            };
            all.onerror = function () { finish(null); };
          } catch (e) { finish(null); }
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

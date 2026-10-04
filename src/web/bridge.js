/*
 * 飞牛桌面桥接适配层（ES Module）。
 *
 * 为什么用官方 SDK 而不是手搓协议：
 *   飞牛桌面把 penpal 的协议枚举改成了「字符串」——
 *     Call="call" / Reply="reply" / Syn="syn" / SynAck="synAck" / Ack="ack"
 *   与 npm 上 penpal 库的数字枚举（Syn=0 / Call=3）**不兼容**。
 *   而且握手是「三回合」，子端收到 SYN-ACK 之后**必须回 ACK**
 *   （{penpal:"ack", methodNames:[子端方法表], config}），宿主才会绑定 RPC 通道。
 *   上面任意一条不满足，宿主都是「一声不吭」—— 前端侧收不到任何错误，表现就是
 *   「点了没用」。这里直接 vendor 官方 @trimjs/web-app（v0.4.2），协议、时序、
 *   多运行时差异全部交给官方兜住，我们只做一层薄适配。
 *
 * 对外契约（挂在 window.fnosBridge，供 app.js 这个传统脚本消费）：
 *   ready()                     -> Promise<{ok, reason, methods[], error}>
 *                                  等握手结束（**自带超时**，见下方注释）
 *   has(name)                   -> boolean     宿主是否公开了该方法
 *   call(name, ...args)         -> Promise     转发调用（优先走 SDK 包装方法）
 *   openApp(anchor)             -> Promise     唤起桌面子窗口入口
 *   setTitle(title)             -> Promise
 *   close()                     -> Promise
 *   openURL(url, target, feat)  -> Promise
 *   describe()                  -> object      诊断快照（排查用）
 *
 * ⚠️ 坑：SDK 的 initPromise 在握手失败时「既不 resolve 也不 reject」
 *   （它的 init() 里只挂了 .then，没有 .catch），所以这一层必须自己加超时，
 *   否则 await ready() 会永久挂起，表现为「独立窗口永远打不开且没有任何提示」。
 */
import { TrimApp } from './vendor/trimjs-web-app.js?v=__MV_VERSION__';

var HANDSHAKE_TIMEOUT_MS = 3000;

// 本应用关心的宿主方法（仅用于诊断输出，不做硬编码开关）
var WANTED = ['openApp', 'openCustomApp', 'setTitle', 'close', 'openURL'];

var sdk = null;
var via = null;
var sdkErr = null;
var readyPromise = null;

function log() {
  var args = ['[mediaview-bridge]'].concat(Array.prototype.slice.call(arguments));
  try { console.log.apply(console, args); } catch (e) { /* 忽略 */ }
}

// 构造 SDK；优先官方 npm 包，其次宿主注入的全局构造器
function getSdk() {
  if (sdk) return sdk;
  if (sdkErr) return null;
  try {
    sdk = new TrimApp();
    via = 'npm:@trimjs/web-app';
    log('SDK 就绪, via=' + via, {
      isWeb: sdk.isWeb,
      isStandaloneWeb: sdk.isStandaloneWeb,
    });
    return sdk;
  } catch (e) {
    log('new TrimApp() 失败，尝试宿主注入的构造器:', e && e.message);
  }
  // 兜底：某些宿主会把构造器挂到全局
  var scopes = [];
  var push = function (s) { if (s && scopes.indexOf(s) < 0) scopes.push(s); };
  push(window);
  try { push(window.parent); } catch (e) { /* 跨域 */ }
  for (var i = 0; i < scopes.length; i++) {
    var sc = scopes[i];
    for (var j = 0; j < 2; j++) {
      var name = j === 0 ? 'TrimApp' : 'FnApp';
      if (typeof sc[name] === 'function') {
        try {
          sdk = new (sc[name])();
          via = 'host-injected:' + name;
          log('SDK 就绪, via=' + via);
          return sdk;
        } catch (e2) { /* 继续 */ }
      }
    }
  }
  sdkErr = new Error('SDK_UNAVAILABLE');
  log('SDK 不可用：既拿不到 npm 包，宿主也没有注入构造器');
  return null;
}

// 宿主方法表（可能抛错：不在 iframe / 握手未完成 / 宿主不支持）
function hostMethods() {
  var s = getSdk();
  if (!s) return null;
  try {
    var m = s.getWebMethods();
    if (!m || typeof m !== 'object') return null;
    return m;
  } catch (e) {
    return null;
  }
}

function methodNames() {
  var m = hostMethods();
  if (!m) return [];
  var out = [];
  for (var k in m) {
    if (Object.prototype.hasOwnProperty.call(m, k) && typeof m[k] === 'function') out.push(k);
  }
  return out;
}

/**
 * 等握手结束。**始终会 settle**，不会永久挂起。
 * reason 取值：ok / sdk-unavailable / standalone / handshake-timeout /
 *              handshake-failed / not-web-runtime / no-host-bridge / empty-host-methods
 */
function ready() {
  if (readyPromise) return readyPromise;
  readyPromise = new Promise(function (resolve) {
    var s = getSdk();
    if (!s) return resolve({ ok: false, reason: 'sdk-unavailable', methods: [], error: 'SDK_UNAVAILABLE' });

    // 不在 iframe 里（直接开标签页 / PC 客户端）：SDK 会跳过宿主桥接初始化
    if (s.isStandaloneWeb) {
      log('standalone：window.parent === window，宿主桥接不适用');
      return resolve({ ok: false, reason: 'standalone', methods: [] });
    }

    var done = false;
    function finish(r) {
      if (done) return;
      done = true;
      clearTimeout(timer);
      // 失败不缓存：否则一次握手超时会让整个页面会话都用不了宿主桥接，
      // 用户只能刷新页面才能重试（表现为"独立窗口永远打不开"）。
      if (!r.ok) readyPromise = null;
      resolve(r);
    }

    var timer = setTimeout(function () {
      log('握手超时（' + HANDSHAKE_TIMEOUT_MS + 'ms），判定宿主桥接不可用');
      finish({ ok: false, reason: 'handshake-timeout', methods: [] });
    }, HANDSHAKE_TIMEOUT_MS);

    var p;
    try {
      p = s.ready();
    } catch (e) {
      return finish({ ok: false, reason: 'handshake-failed', methods: [], error: String((e && e.message) || e) });
    }

    Promise.resolve(p).then(function () {
      if (!s.isWeb) {
        log('非 web 运行时（移动端），不具备 openApp');
        return finish({ ok: false, reason: 'not-web-runtime', methods: [] });
      }
      var names = methodNames();
      if (!names.length) {
        log('拿不到宿主方法表（getWebMethods 抛错或为空）');
        return finish({ ok: false, reason: 'empty-host-methods', methods: [] });
      }
      log('握手完成, 宿主方法表 =', names);
      finish({ ok: true, reason: 'ok', methods: names });
    }, function (e) {
      log('握手失败:', e && e.message);
      finish({ ok: false, reason: 'handshake-failed', methods: [], error: String((e && e.message) || e) });
    });
  });
  return readyPromise;
}

function has(name) {
  return methodNames().indexOf(name) >= 0;
}

/**
 * 转发调用。
 * 优先走 SDK 的包装方法（它会顺带处理多运行时差异，例如 setTitle 同时改 document.title）；
 * SDK 上没有包装的，直接透传到宿主方法代理。
 */
function call(name) {
  var args = Array.prototype.slice.call(arguments, 1);
  var s = getSdk();
  if (!s) return Promise.reject(new Error('SDK_UNAVAILABLE'));
  if (typeof s[name] === 'function') {
    try {
      return Promise.resolve(s[name].apply(s, args));
    } catch (e) {
      return Promise.reject(e);
    }
  }
  var m = hostMethods();
  if (m && typeof m[name] === 'function') {
    try {
      return Promise.resolve(m[name].apply(m, args));
    } catch (e) {
      return Promise.reject(e);
    }
  }
  return Promise.reject(new Error('宿主没有公开方法: ' + name));
}

function openApp(anchor) { return call('openApp', anchor); }
function setTitle(t) { return call('setTitle', t); }
function close() { return call('close'); }
function openURL(url, target, features) { return call('openURL', url, target, features); }

function describe() {
  var s = getSdk();
  var names = methodNames();
  var wanted = {};
  for (var i = 0; i < WANTED.length; i++) wanted[WANTED[i]] = names.indexOf(WANTED[i]) >= 0;
  return {
    sdkLoaded: !!s,
    via: via,
    isTop: window === window.top,
    isStandaloneWeb: s ? s.isStandaloneWeb : undefined,
    isWeb: s ? s.isWeb : undefined,
    methodCount: names.length,
    methods: names,
    wanted: wanted,
    error: sdkErr ? String(sdkErr.message) : null,
  };
}

var api = {
  ready: ready,
  has: has,
  call: call,
  openApp: openApp,
  setTitle: setTitle,
  close: close,
  openURL: openURL,
  describe: describe,
  HANDSHAKE_TIMEOUT_MS: HANDSHAKE_TIMEOUT_MS,
};

window.fnosBridge = api;
log('桥接层已挂载到 window.fnosBridge');

export default api;

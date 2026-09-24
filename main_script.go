package main

import "strings"

// mainPageScript returns the browser-side JavaScript for the combined page.
//
// The key handling is deliberate: CPA's management endpoints authenticate from a
// request header and the resource route is GET-only, so the key lives in
// localStorage and is attached by fetch(). It never reaches the plugin.
func mainPageScript() string {
	const script = `<script>
{{UI_TABS}}
(function () {
  var KEY_NAME = '{{KEY_NAME}}';
  var MGMT = '{{MGMT}}';
  var BASE = '{{BASE}}';

  function key() {
    try { return localStorage.getItem(KEY_NAME) || ''; } catch (e) { return ''; }
  }

  function setKeyState(msg, cls) {
    var el = document.getElementById('keyState');
    if (!el) return;
    el.textContent = msg || '';
    el.className = 'muted small ' + (cls || '');
  }

  function refreshKeyState() {
    var k = key();
    if (k) setKeyState('已保存密钥（' + k.length + ' 字符），操作可直接使用。', 'ok');
    else setKeyState('尚未保存密钥，涉及数据的操作会提示缺少管理密钥。', 'warn');
  }

  window.saveKey = function () {
    var input = document.getElementById('mgmtKey');
    var v = (input && input.value || '').trim();
    if (!v) { setKeyState('请输入密钥', 'bad'); return; }
    try { localStorage.setItem(KEY_NAME, v); } catch (e) {
      setKeyState('浏览器拒绝保存（可能禁用了 localStorage）', 'bad'); return;
    }
    if (input) input.value = '';
    refreshKeyState();
  };

  window.clearKey = function () {
    try { localStorage.removeItem(KEY_NAME); } catch (e) {}
    refreshKeyState();
  };

  function call(path, options) {
    var k = key();
    if (!k) { return Promise.reject(new Error('请先在「设置」里保存管理密钥')); }
    var opts = options || {};
    opts.headers = Object.assign({
      'Authorization': 'Bearer ' + k,
      'X-Management-Key': k
    }, opts.headers || {});
    return fetch(path, opts).then(function (resp) {
      return resp.text().then(function (body) {
        var data = null;
        try { data = JSON.parse(body); } catch (e) {}
        if (!resp.ok) {
          var msg = (data && (data.error || data.message)) || ('HTTP ' + resp.status);
          if (String(msg).indexOf('management key') >= 0) {
            msg = '管理密钥无效或未配置（' + msg + '）';
          }
          throw new Error(msg);
        }
        return data;
      });
    });
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function msgSet(id, text, cls) {
    var el = document.getElementById(id);
    if (!el) return;
    el.textContent = text || '';
    el.className = 'small ' + (cls || 'muted');
  }

  function renderCheckin(run) {
    if (!run) return '';
    var rs = run.results || [];
    var out = '<table><thead><tr><th>账号</th><th>结果</th><th>说明</th><th class="num">码</th></tr></thead><tbody>';
    if (!rs.length) {
      out += '<tr><td colspan="4" class="muted">无结果</td></tr>';
    }
    rs.forEach(function (r) {
      var pill = 'ok', text = '成功';
      if (r.error) { pill = 'bad'; text = '错误'; }
      else if (!r.success) { pill = 'bad'; text = '失败'; }
      else if (r.already_checked_in) { pill = 'warn'; text = '已签到'; }
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td><span class="pill ' + pill + '">' + text + '</span></td>' +
        '<td>' + esc(r.error || r.message) + '</td>' +
        '<td class="num">' + (r.code == null ? '' : r.code) + '</td></tr>';
    });
    out += '</tbody></table>';
    return out;
  }

  function renderQuota(results) {
    if (!results || !results.length) return '';
    var out = '<table><thead><tr><th>账号</th><th>区域</th><th class="num">剩余积分</th><th>说明</th></tr></thead><tbody>';
    results.forEach(function (r) {
      var cls = r.error ? 'bad' : 'ok';
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td><span class="pill idle">' + esc(r.region) + '</span></td>' +
        '<td class="num ' + cls + '">' + (r.credits == null ? 0 : r.credits) + '</td>' +
        '<td>' + esc(r.error || r.message) + '</td></tr>';
    });
    out += '</tbody></table>';
    return out;
  }

  // ---- combined action ------------------------------------------------
  window.runAll = function () {
    var btn = document.getElementById('btnRun');
    var box = document.getElementById('runResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    msgSet('runMsg', '执行中…', 'muted');

    call(BASE + '/run', { method: 'POST' }).then(function (payload) {
      var c = payload.checkin || {};
      msgSet('runMsg', '完成：签到成功 ' + (c.succeeded || 0) + ' / 失败 ' + (c.failed || 0), 'ok');
      if (box) {
        box.innerHTML = '<h2>本次结果</h2>' +
          (payload.checkin ? renderCheckin(payload.checkin) : '') +
          (payload.quota ? renderQuota(payload.quota) : '');
      }
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) {
      msgSet('runMsg', '执行失败：' + e.message, 'bad');
    }).then(function () { if (btn) btn.disabled = false; });
  };

  window.refreshAccounts = function () {
    msgSet('runMsg', '刷新中…', 'muted');
    call(BASE + '/accounts').then(function (d) {
      msgSet('runMsg', '账号 ' + (d.total || 0) + ' 个，可用 ' + (d.usable || 0) + ' 个', 'ok');
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) { msgSet('runMsg', '刷新失败：' + e.message, 'bad'); });
  };

  // ---- strategy --------------------------------------------------------
  window.saveStrategy = function () {
    var picked = document.querySelector('input[name="strategy"]:checked');
    if (!picked) { msgSet('strategyMsg', '请选择一种策略', 'bad'); return; }
    msgSet('strategyMsg', '应用中…', 'muted');
    call(BASE + '/routing/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ strategy: picked.value })
    }).then(function () {
      msgSet('strategyMsg', '已应用：' + picked.value, 'ok');
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) { msgSet('strategyMsg', '应用失败：' + e.message, 'bad'); });
  };

  window.resetRotation = function () {
    msgSet('strategyMsg', '重置中…', 'muted');
    call(BASE + '/routing/reset', { method: 'POST' }).then(function () {
      msgSet('strategyMsg', '轮巡位置已重置', 'ok');
    }).catch(function (e) { msgSet('strategyMsg', '重置失败：' + e.message, 'bad'); });
  };

  // ---- check-in --------------------------------------------------------
  window.runCheckin = function () {
    msgSet('runMsg', '签到中…', 'muted');
    call(BASE + '/checkin/run', { method: 'POST' }).then(function (run) {
      msgSet('runMsg', '签到完成：成功 ' + (run.succeeded || 0) + ' / 失败 ' + (run.failed || 0), 'ok');
      var box = document.getElementById('runResult');
      if (box) box.innerHTML = '<h2>签到结果</h2>' + renderCheckin(run);
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) { msgSet('runMsg', '签到失败：' + e.message, 'bad'); });
  };

  window.saveCheckinSettings = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/checkin/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        enabled: !!document.getElementById('ckEnabled').checked,
        hour: parseInt(document.getElementById('ckHour').value, 10) || 0,
        minute: parseInt(document.getElementById('ckMinute').value, 10) || 0,
        on_start: !!document.getElementById('ckOnStart').checked
      })
    }).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- quota -----------------------------------------------------------
  window.refreshQuota = function () {
    msgSet('runMsg', '查询中…', 'muted');
    call(BASE + '/quota/refresh', { method: 'POST' }).then(function (payload) {
      msgSet('runMsg', '完成：积分合计 ' + (payload.total_credits || 0) +
        '（' + (payload.accounts_known || 0) + '/' + (payload.accounts_total || 0) + ' 账号已查询）', 'ok');
      var box = document.getElementById('runResult');
      if (box) box.innerHTML = '<h2>积分结果</h2>' + renderQuota(payload.results);
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) { msgSet('runMsg', '刷新失败：' + e.message, 'bad'); });
  };

  window.saveQuotaSettings = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/quota/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        enabled: !!document.getElementById('qEnabled').checked,
        interval_minutes: parseInt(document.getElementById('qInterval').value, 10) || 30,
        refresh_on_start: !!document.getElementById('qOnStart').checked
      })
    }).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- variant override -----------------------------------------------
  window.setVariant = function (v) {
    var msg = document.getElementById('variantMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/variant', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ variant: v })
    }).then(function () {
      if (msg) { msg.textContent = '已切换为 ' + (v || '自动') + '，刷新列表后生效'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) {
      if (msg) { msg.textContent = '设置失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- account toggle --------------------------------------------------
  window.toggleAccount = function (uid, action, authIndex) {
    msgSet('runMsg', '操作中…', 'muted');
    call(BASE + '/account/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: uid, auth_index: authIndex || '', action: action || 'toggle' })
    }).then(function () {
      msgSet('runMsg', '已完成', 'ok');
      setTimeout(function () { location.reload(); }, 500);
    }).catch(function (e) {
      msgSet('runMsg', '操作失败：' + e.message, 'bad');
    });
  };

  document.addEventListener('DOMContentLoaded', function () {
    refreshKeyState();
    restoreTab();
  });
  if (document.readyState !== 'loading') { refreshKeyState(); restoreTab(); }
})();
</script>`

	return strings.NewReplacer(
		"{{KEY_NAME}}", checkinKeyStorageName,
		"{{MGMT}}", managementBasePath()+"/"+pluginName,
		"{{BASE}}", managementBasePath()+"/"+pluginName,
		"{{UI_TABS}}", uiTabsScript,
	).Replace(script)
}

package main

import "strings"

// quotaPageScript returns the browser-side JavaScript for the quota page.
//
// Same rationale as the check-in page: CPA's management endpoints authenticate
// from a request header, and the resource route the page loads from is GET-only,
// so the key is kept in localStorage and attached by fetch(). The key never
// reaches the plugin.
func quotaPageScript() string {
	// Interpolate the Go-side constants through placeholders so the JS body can
	// be a single raw string literal (a backtick inside would terminate it).
	const script = `<script>
(function () {
  var KEY_NAME = '{{KEY_NAME}}';
  var MGMT = '{{MGMT}}';

  function key() {
    try { return localStorage.getItem(KEY_NAME) || ''; } catch (e) { return ''; }
  }

  function setState(msg, cls) {
    var el = document.getElementById('keyState');
    if (!el) return;
    el.textContent = msg || '';
    el.className = 'muted ' + (cls || '');
  }

  function refreshKeyState() {
    var k = key();
    if (k) {
      setState('已保存密钥（' + k.length + ' 字符），按钮可直接使用。', 'ok');
    } else {
      setState('尚未保存密钥，刷新操作会返回 “missing management key”。', 'warn');
    }
  }

  window.saveKey = function () {
    var input = document.getElementById('mgmtKey');
    var v = (input && input.value || '').trim();
    if (!v) { setState('请输入密钥', 'bad'); return; }
    try { localStorage.setItem(KEY_NAME, v); } catch (e) {
      setState('浏览器拒绝保存（可能禁用了 localStorage）', 'bad'); return;
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
    if (!k) { return Promise.reject(new Error('请先在上方保存管理密钥')); }
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

  function renderResults(payload) {
    var rs = payload.results || [];
    if (!rs.length) { return '<p class="muted">没有可查询的账号。</p>'; }
    var out = '<h2>账号额度</h2><div class="card"><table>' +
      '<tr><th>账号</th><th>区域</th><th>剩余额度</th><th>说明</th></tr>';
    rs.forEach(function (r) {
      var cls = r.error ? 'bad' : 'ok';
      var note = r.error || r.message || '';
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td>' + esc(r.region) + '</td>' +
        '<td class="' + cls + '">' + (r.credits == null ? 0 : r.credits) + '</td>' +
        '<td>' + esc(note) + '</td></tr>';
    });
    out += '</table></div>';
    return out;
  }

  window.refreshQuota = function () {
    var btn = document.getElementById('btnRefresh');
    var msg = document.getElementById('runMsg');
    var box = document.getElementById('runResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    if (msg) { msg.textContent = '查询中，请稍候…'; msg.className = 'muted'; }

    call(MGMT + '/refresh', { method: 'POST' }).then(function (payload) {
      var total = payload.total_credits || 0;
      var known = payload.accounts_known || 0;
      var all = payload.accounts_total || 0;
      if (msg) {
        msg.textContent = '完成：已知额度合计 ' + total + '（' + known + '/' + all + ' 账号已查询）';
        msg.className = 'ok';
      }
      if (box) box.innerHTML = renderResults(payload);
    }).catch(function (e) {
      if (msg) { msg.textContent = '刷新失败：' + e.message; msg.className = 'bad'; }
    }).then(function () {
      if (btn) btn.disabled = false;
    });
  };

  window.saveConfig = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'muted'; }
    var payload = {
      enabled: !!document.getElementById('qEnabled').checked,
      interval_minutes: parseInt(document.getElementById('qInterval').value, 10) || 30,
      refresh_on_start: !!document.getElementById('qOnStart').checked
    };
    call(MGMT + '/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'ok'; }
      setTimeout(function () { location.reload(); }, 600);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'bad'; }
    });
  };

  document.addEventListener('DOMContentLoaded', refreshKeyState);
  if (document.readyState !== 'loading') refreshKeyState();
})();
</script>`

	out := strings.NewReplacer(
		"{{KEY_NAME}}", checkinKeyStorageName,
		"{{MGMT}}", managementBasePath()+"/"+pluginName+"/quota",
	).Replace(script)
	return out
}

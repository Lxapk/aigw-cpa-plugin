package main

// checkinPageScript returns the browser-side JavaScript for the check-in page.
//
// Why JavaScript instead of plain HTML forms:
//
//	CPA's management endpoints authenticate from an HTTP header
//	(Authorization: Bearer <key> or X-Management-Key, see
//	internal/api/handlers/management/handler.go:276). An HTML form cannot set
//	request headers, and the resource route the page is served from is GET-only.
//	So the page keeps the key in localStorage and uses fetch() to attach it.
//
// The key is never posted to the plugin.
func checkinPageScript() string {
	return `<script>
(function () {
  var KEY_NAME = '` + checkinKeyStorageName + `';
  var MGMT = '` + managementBasePath() + `/` + pluginName + `/checkin';

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
      setState('尚未保存密钥，签到相关操作会返回 “missing management key”。', 'warn');
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
    if (!k) {
      return Promise.reject(new Error('请先在上方保存管理密钥'));
    }
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

  function renderRun(run) {
    var trigger = { manual: '手动', auto: '自动', startup: '启动补跑' }[run.trigger] || run.trigger || '';
    var out = '<div class="card"><div class="muted">' + esc(trigger) +
      ' · 成功 ' + (run.succeeded || 0) + ' / 失败 ' + (run.failed || 0) + '</div>';
    var rs = run.results || [];
    if (!rs.length) {
      out += '<p class="muted">无结果</p></div>';
      return out;
    }
    out += '<table><tr><th>账号</th><th>UID</th><th>结果</th><th>说明</th><th>码</th></tr>';
    rs.forEach(function (r) {
      var cls = 'ok', text = '成功';
      if (r.error) { cls = 'bad'; text = '错误'; }
      else if (!r.success) { cls = 'bad'; text = '失败'; }
      else if (r.already_checked_in) { cls = 'warn'; text = '已签到'; }
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td><code>' + esc(r.uid || r.auth_id) + '</code></td>' +
        '<td class="' + cls + '">' + text + '</td>' +
        '<td>' + esc(r.error || r.message) + '</td>' +
        '<td>' + (r.code == null ? '' : r.code) + '</td></tr>';
    });
    out += '</table></div>';
    return out;
  }

  window.saveConfig = function () {
    var msg = document.getElementById('runMsg');
    if (msg) msg.textContent = '保存中…';
    var payload = {
      enabled: !!document.getElementById('ckEnabled').checked,
      hour: parseInt(document.getElementById('ckHour').value, 10) || 0,
      minute: parseInt(document.getElementById('ckMinute').value, 10) || 0,
      on_start: !!document.getElementById('ckOnStart').checked
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

  window.runCheckin = function () {
    var btn = document.getElementById('btnRun');
    var msg = document.getElementById('runMsg');
    var box = document.getElementById('runResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    if (msg) { msg.textContent = '签到中，请稍候…'; msg.className = 'muted'; }

    call(MGMT + '/run', { method: 'POST' }).then(function (run) {
      if (msg) { msg.textContent = '完成：成功 ' + (run.succeeded || 0) + ' / 失败 ' + (run.failed || 0); msg.className = 'ok'; }
      if (box) box.innerHTML = '<h2>本次结果</h2>' + renderRun(run);
    }).catch(function (e) {
      if (msg) { msg.textContent = '签到失败：' + e.message; msg.className = 'bad'; }
    }).then(function () {
      if (btn) btn.disabled = false;
    });
  };

  document.addEventListener('DOMContentLoaded', refreshKeyState);
  if (document.readyState !== 'loading') refreshKeyState();
})();
</script>`
}

// A short peer lease ensures only one node hosts an active management session.
(() => {
  async function keepalive() {
    try {
      const response = await fetch('./cluster/session/keepalive', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'});
      if (response.redirected || response.status === 401) location.href = './login';
    } catch (_) { /* The next request retries; the server independently expires the lease. */ }
  }
  keepalive();
  setInterval(keepalive, 15000);
})();

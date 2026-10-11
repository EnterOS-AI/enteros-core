const assert = require('node:assert/strict');
const net = require('node:net');
const { test } = require('node:test');
const { pickPort } = require('./lib/dev-port.cjs');

for (const host of ['127.0.0.1', '0.0.0.0', '::']) {
  test(`does not reuse an occupied ${host} port`, async (t) => {
    const server = net.createServer();
    try {
      await new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen({ host, port: 0, exclusive: true }, resolve);
      });
    } catch (error) {
      if (host === '::' && ['EAFNOSUPPORT', 'EADDRNOTAVAIL'].includes(error.code)) {
        t.skip('IPv6 unavailable'); return;
      }
      throw error;
    }
    try {
      const occupied = server.address().port;
      assert.notEqual(await pickPort(occupied), occupied);
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });
}

test('reuses an available preferred port', async () => {
  const available = await pickPort(0);
  assert.equal(await pickPort(available), available);
});

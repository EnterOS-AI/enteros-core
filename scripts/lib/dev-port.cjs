// Check existing listeners as well as bind/listen: Windows can allow a more
// specific loopback bind alongside another process's wildcard listener.
// Node is already required by Canvas.
const net = require('node:net');

function hasListener(host, port) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port });
    const finish = (busy) => { socket.destroy(); resolve(busy); };
    socket.once('connect', () => finish(true));
    socket.once('error', () => finish(false));
    // A nonresponsive port is not safe to hand to a new service either.
    socket.setTimeout(300, () => finish(true));
  });
}

async function pickPort(preferred) {
  for (let attempt = 0; attempt < 20; attempt++) {
    const listeners = [];
    let port = attempt === 0 ? preferred : 0;
    try {
      if (port && (await Promise.all(['127.0.0.1', '::1'].map((host) => hasListener(host, port)))).some(Boolean)) continue;
      for (const host of ['127.0.0.1', '::1']) {
        if (host === '::1' && await hasListener(host, port)) {
          throw Object.assign(new Error('IPv6 port is occupied'), { code: 'EADDRINUSE' });
        }
        const server = net.createServer();
        try {
          await new Promise((resolve, reject) => {
            server.once('error', reject);
            server.listen({ host, port, exclusive: true, ipv6Only: host === '::1' }, resolve);
          });
        } catch (error) {
          if (host === '::1' && ['EAFNOSUPPORT', 'EADDRNOTAVAIL'].includes(error.code)) continue;
          throw error;
        }
        listeners.push(server);
        port = server.address().port;
      }
      return port;
    } catch (error) {
      if (!['EADDRINUSE', 'EACCES'].includes(error.code)) throw error;
    } finally {
      await Promise.all(listeners.map((server) => new Promise((resolve) => server.close(resolve))));
    }
  }
  throw new Error('Could not find a free local port');
}

module.exports = { pickPort };
if (require.main === module) {
  const preferred = Number(process.argv[2]);
  if (!Number.isInteger(preferred) || preferred < 0 || preferred > 65535) {
    console.error('Expected a port between 0 and 65535');
    process.exitCode = 2;
  } else {
    pickPort(preferred).then(console.log).catch((error) => {
      console.error(error.message);
      process.exitCode = 1;
    });
  }
}

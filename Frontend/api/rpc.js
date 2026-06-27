export default async function handler(req, res) {
  const path = req.url.replace(/^\/api\/rpc/, '');
  const target = 'https://prax.val-a.grad.dev.app.canopynetwork.org/rpc' + path;
  try {
    const response = await fetch(target, {
      method: req.method,
      headers: { 'Content-Type': 'application/json' },
      body: req.method === 'POST' ? JSON.stringify(req.body) : undefined,
    });
    const data = await response.json();
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.status(200).json(data);
  } catch(e) {
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.status(500).json({ error: e.message });
  }
}

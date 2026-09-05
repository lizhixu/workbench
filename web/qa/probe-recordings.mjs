const B = 'http://192.255.178.173'
const login = await fetch(B + '/api/v1/auth/login', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ username: 'admin', password: 'admin' }),
})
const token = (await login.json()).token
const s = await fetch(B + '/api/v1/sessions', { headers: { Authorization: `Bearer ${token}` } })
const list = (await s.json()).data || []
console.log('会话数:', list.length)
for (const x of list.slice(0, 8)) {
  const r = await fetch(B + '/api/v1/sessions/' + x.id + '/recording?token=' + encodeURIComponent(token), {
    signal: AbortSignal.timeout(30000),
  })
  const body = await r.text()
  console.log(x.id, 'live=' + x.live, 'dur=' + x.duration_sec + 's', 'HTTP=' + r.status, 'bytes=' + body.length, JSON.stringify(body.slice(0, 100)))
}

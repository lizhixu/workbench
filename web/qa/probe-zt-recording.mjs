const B='http://192.255.178.173'
const login=await fetch(B+'/api/v1/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:'admin',password:'admin'})})
const token=(await login.json()).token
const sr=await fetch(B+'/api/v1/sessions',{headers:{Authorization:'Bearer '+token}})
const sessions=(await sr.json()).data||[]
for(const s of sessions.filter(x=>x.hostname==='ZT2285135695').slice(0,10)){
  const r=await fetch(B+'/api/v1/sessions/'+s.id+'/recording?token='+encodeURIComponent(token))
  const text=await r.text()
  const lines=text.trim().split('\n')
  let events=[]
  try{JSON.parse(lines[0]); events=lines.slice(1).filter(Boolean).map(x=>JSON.parse(x))}catch{}
  console.log(JSON.stringify({id:s.id,started:s.started_at,ended:s.ended_at,duration:s.duration_sec,http:r.status,bytes:text.length,lines:lines.length,events:events.length,first:events.slice(0,3),last:events.slice(-2)}))
}

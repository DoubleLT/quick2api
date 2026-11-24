const test=require('node:test'),assert=require('node:assert/strict');
const {MessageSSE,post}=require('./client.cjs');
const http=require('node:http');
const events=[
 {type:'message_start',message:{id:'fixture',content:[],stop_reason:null}},
 {type:'content_block_start',index:0,content_block:{type:'text',text:''}},
 {type:'content_block_delta',index:0,delta:{type:'text_delta',text:'先读取 🧪'}},
 {type:'content_block_stop',index:0},
 {type:'content_block_start',index:1,content_block:{type:'tool_use',id:'call_fixture',name:'Read',input:{}}},
 {type:'content_block_delta',index:1,delta:{type:'input_json_delta',partial_json:'{"file_path":"D:/fixture/'}},
 {type:'content_block_delta',index:1,delta:{type:'input_json_delta',partial_json:'测试.txt"}'}},
 {type:'content_block_stop',index:1},
 {type:'message_delta',delta:{stop_reason:'tool_use'}},
 {type:'message_stop'},
];
function encode(items,newline='\n'){return items.map(event=>'event: '+event.type+newline+'data: '+JSON.stringify(event)+newline+newline).join('');}
test('SSE reconstructs fragmented UTF-8 text and JSON inputs across CRLF boundaries',()=>{
 const raw=encode(events,'\r\n');
 for(const size of [1,2,7,53,raw.length]){
  const parser=new MessageSSE();for(let at=0;at<raw.length;at+=size)parser.feed(raw.slice(at,at+size));
  const reply=parser.finish();assert.equal(reply.stop_reason,'tool_use');assert.equal(reply.content[0].text,'先读取 🧪');
  assert.deepEqual(reply.content[1].input,{file_path:'D:/fixture/测试.txt'});
 }
});
test('partial or structurally corrupt tool delivery cannot be executed',()=>{
 for(const cut of [5,6,7,8,9]){
  const parser=new MessageSSE();parser.feed(encode(events.slice(0,cut)));assert.throws(()=>parser.finish(),/Incomplete/);
 }
 const duplicate=new MessageSSE();assert.throws(()=>duplicate.feed(encode([events[0],events[0]])),/Duplicate/);
 const error=new MessageSSE();assert.throws(()=>error.feed(encode([{type:'error',error:{type:'api_error'}}])),/upstream error/);
 const ended=new MessageSSE();ended.feed(encode(events));assert.throws(()=>ended.feed(encode([events[1]])),/after message_stop/);
});

test('HTTP and SSE errors retain private evidence and safe status metadata',async t=>{
 const raw='{"error":{"type":"fixture_error","message":"PRIVATE fixture details"}}';
 const srv=http.createServer((req,res)=>{
  if(req.url==='/http'){res.writeHead(502,{'x-should-retry':'false'});res.end(raw);}
  else {res.writeHead(200,{'content-type':'text/event-stream'});res.end(encode([{type:'error',error:{type:'api_error',message:'PRIVATE fixture details'}}]));}
 });
 await new Promise(resolve=>srv.listen(0,'127.0.0.1',resolve));t.after(()=>srv.close());
 for(const pathname of ['/http','/sse']){
  await assert.rejects(post({endpoint:'http://127.0.0.1:'+srv.address().port+pathname,key:'fixture',session:'fixture',payload:{stream:pathname==='/sse'}}),error=>{
   assert.equal(error.evidence.status,pathname==='/http'?502:200);assert.match(error.evidence.raw,/PRIVATE fixture details/);
   assert.equal(error.evidence.shouldRetry,pathname==='/http'?'false':undefined);
   assert.doesNotMatch(error.message,/PRIVATE/);assert.match(error.evidence.requestId,/^regression-/);
   if(pathname==='/sse')assert.equal(error.evidence.code,'api_error');return true;
  });
 }
});

test('wire client preserves split multibyte UTF-8 and reports caller cancellation',async t=>{
 const srv=http.createServer((req,res)=>{
  res.writeHead(200,{'content-type':'text/event-stream'});res.flushHeaders();
  if(req.url==='/cancel'){req.on('close',()=>res.destroy());return;}
  for(const byte of Buffer.from(encode(events)))res.write(Buffer.from([byte]));res.end();
 });
 await new Promise(resolve=>srv.listen(0,'127.0.0.1',resolve));t.after(()=>srv.close());
 const endpoint='http://127.0.0.1:'+srv.address().port;
 const out=await post({endpoint:endpoint+'/utf8',key:'fixture',session:'fixture',payload:{stream:true}});
 assert.equal(out.reply.content[0].text,'先读取 🧪');
 const controller=new AbortController();
 await assert.rejects(post({endpoint:endpoint+'/cancel',key:'fixture',session:'fixture',payload:{stream:true},signal:controller.signal,onHeaders:()=>controller.abort()}),error=>error.name==='AbortError'&&error.evidence.status===200);
});

test('malformed response, SSE event and tool JSON do not disclose response text',async t=>{
 const malformed={
  '/json':'{"PRIVATE_SECRET": invalid}',
  '/event':'data: {"PRIVATE_SECRET": invalid}\n\n',
  '/input':encode([events[0],events[4],{type:'content_block_delta',index:1,delta:{type:'input_json_delta',partial_json:'{"PRIVATE_SECRET": invalid}'}},events[7]]),
 };
 const srv=http.createServer((req,res)=>{res.writeHead(200);res.end(malformed[req.url]);});
 await new Promise(resolve=>srv.listen(0,'127.0.0.1',resolve));t.after(()=>srv.close());
 for(const pathname of Object.keys(malformed))await assert.rejects(post({endpoint:'http://127.0.0.1:'+srv.address().port+pathname,key:'fixture',session:'fixture',payload:{stream:pathname!=='/json'}}),error=>{
  assert.doesNotMatch(error.message,/PRIVATE_SECRET/);assert.match(error.message,/Malformed/);assert.equal(error.code,'invalid_json');assert.match(error.evidence.raw,/PRIVATE_SECRET/);return true;
 });
});

test('SSE heartbeats cannot extend the total request deadline',async t=>{
 const srv=http.createServer((req,res)=>{
  res.writeHead(200,{'content-type':'text/event-stream'});res.flushHeaders();
  const timer=setInterval(()=>res.write(': heartbeat\n\n'),5);
  res.on('close',()=>clearInterval(timer));
 });
 await new Promise(resolve=>srv.listen(0,'127.0.0.1',resolve));t.after(()=>srv.close());
 await assert.rejects(post({endpoint:'http://127.0.0.1:'+srv.address().port,key:'fixture',session:'fixture',payload:{stream:true},timeoutMs:80}),error=>/deadline exceeded/.test(error.message)&&error.evidence.status===200);
});

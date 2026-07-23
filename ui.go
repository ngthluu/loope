package main

// uiJS is the dashboard's client script, kept out of pageTmpl so it contains no
// template actions and can be exercised directly by ui_js_test.go under node.
//
// The polling model is deliberately *patch*, not *replace*. Re-assigning
// innerHTML on every poll re-creates every node in the pane, which replays the
// .fadein entrance animation across the whole screen, drops text selection and
// focus, and resets both disclosure state and scroll offsets — so a single new
// transcript line looked like a full page reload. morph() instead walks the
// freshly fetched fragment against the live DOM and reuses every node that is
// still structurally the same, so an appended transcript line inserts exactly
// one row and touches nothing else.
//
// Written as a classic script (no template literals, no let/const) because it is
// inlined into a Go raw string literal, which cannot contain a backtick.
const uiJS = `
 // keyOf returns a node's stable identity within its parent, or null when it has
 // none and must be matched positionally.
 function keyOf(n){return n.nodeType===1?n.getAttribute('data-k'):null;}

 // sameType reports whether a live node can be reused for a new one.
 function sameType(a,b){
  if(a.nodeType!==b.nodeType)return false;
  if(a.nodeType!==1)return true;
  return a.tagName===b.tagName&&keyOf(a)===keyOf(b);
 }

 // ownedByClient is true for attributes the browser/user owns after the first
 // render, which the server fragment must not clobber. A running step ships
 // <details open>, so re-applying it every poll would re-open a disclosure the
 // user just closed.
 function ownedByClient(el,name){return name==='open'&&el.hasAttribute('data-disc');}

 function morphAttrs(live,next){
  var na=next.attributes,i,at;
  for(i=0;i<na.length;i++){at=na[i];
   if(ownedByClient(live,at.name))continue;
   if(live.getAttribute(at.name)!==at.value)live.setAttribute(at.name,at.value);}
  var la=live.attributes,names=[];
  for(i=0;i<la.length;i++)names.push(la[i].name);
  for(i=0;i<names.length;i++){
   if(ownedByClient(live,names[i]))continue;
   if(!next.hasAttribute(names[i]))live.removeAttribute(names[i]);}
 }

 function morphNode(live,next){
  if(live.nodeType!==1){if(live.nodeValue!==next.nodeValue)live.nodeValue=next.nodeValue;return;}
  morphAttrs(live,next);
  morphChildren(live,next);
 }

 // morphChildren reconciles live's children against next's: keyed nodes are
 // matched by data-k wherever they moved to, unkeyed ones positionally, and only
 // genuinely new nodes are inserted. Nodes are adopted out of the next tree, so
 // the caller's list is snapshotted first.
 function morphChildren(live,next){
  var keyed={},i,k,kids=[];
  for(i=0;i<live.childNodes.length;i++){k=keyOf(live.childNodes[i]);if(k!==null&&k!==undefined)keyed[k]=live.childNodes[i];}
  for(i=0;i<next.childNodes.length;i++)kids.push(next.childNodes[i]);
  for(i=0;i<kids.length;i++){
   var nn=kids[i],cur=live.childNodes[i]||null,nk=keyOf(nn),match=null;
   if(nk!==null&&nk!==undefined&&keyed[nk]&&sameType(keyed[nk],nn))match=keyed[nk];
   else if(cur&&keyOf(cur)===null&&sameType(cur,nn))match=cur;
   if(match){if(match!==cur)live.insertBefore(match,cur);morphNode(match,nn);}
   else live.insertBefore(nn,cur);
  }
  while(live.childNodes.length>kids.length)live.removeChild(live.childNodes[live.childNodes.length-1]);
 }

 // morph patches live's subtree to match next's, in place. next's own attributes
 // are ignored: it is only a container for the fetched fragment.
 function morph(live,next){morphChildren(live,next);}

 function copySid(btn){if(btn.dataset.copying)return;var id=btn.getAttribute('data-sid')||'';var orig=btn.textContent;var done=function(){btn.dataset.copying='1';btn.textContent='copied';setTimeout(function(){delete btn.dataset.copying;btn.textContent=orig;},1200);};if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(id).then(done,done);}else{var ta=document.createElement('textarea');ta.value=id;document.body.appendChild(ta);ta.select();try{document.execCommand('copy');}catch(e){}document.body.removeChild(ta);done();}}

 if(typeof document!=='undefined'){
  var railEl=document.getElementById('rail'),mainEl=document.getElementById('main'),ago=document.getElementById('ago'),since=0;
  var setText=function(id,v){var e=document.getElementById(id);if(e&&v!=null)e.textContent=v;};
  var applyMeta=function(root){var m=root.querySelector('#railmeta');if(!m)return;setText('stat-tickets',m.dataset.tickets);setText('stat-running',m.dataset.running);setText('stat-spend',m.dataset.spend);};
  setInterval(function(){since++;if(ago)ago.textContent=since+'s';},1000);
  // Cheap short-circuit: identical bytes mean there is nothing to reconcile at
  // all, so skip parsing the fragment. Compare against the previous *fetched*
  // string, not element.innerHTML, which the browser re-serializes and would
  // never byte-match.
  var lastRail=null,lastDetail=null;
  var txScroll=function(root){var out={};root.querySelectorAll('.txfeed').forEach(function(f){out[f.getAttribute('data-seq')]=f.scrollHeight-f.scrollTop-f.clientHeight<8;});return out;};
  var apply=function(el,html){var t=document.createElement('div');t.innerHTML=html;morph(el,t);};
  window.poll=async function(){try{var sel=new URLSearchParams(location.search).get('issue')||'';var q=sel?('?issue='+sel):'';
    var railP=fetch('/rail'+q).then(function(r){return r.text();});
    var detP=fetch('/detail'+q).then(function(r){return r.text();});
    var railHTML=await railP;
    if(railHTML!==lastRail){lastRail=railHTML;apply(railEl,railHTML);}
    applyMeta(railEl);
    var detHTML=await detP;
    if(detHTML!==lastDetail){lastDetail=detHTML;
      // A feed the user had pinned to the bottom keeps following new lines;
      // one they scrolled up in keeps its offset, which morph preserves for
      // free by never re-creating the element.
      var wasAtBottom=txScroll(mainEl);
      apply(mainEl,detHTML);
      mainEl.querySelectorAll('.txfeed').forEach(function(f){var k=f.getAttribute('data-seq');if(wasAtBottom[k]===undefined||wasAtBottom[k])f.scrollTop=f.scrollHeight;});
    }
    since=0;if(ago)ago.textContent='0s';}catch(e){}};
  setInterval(window.poll,3000);
 }

 if(typeof module!=='undefined'&&module.exports)module.exports={morph:morph,morphChildren:morphChildren,keyOf:keyOf};
`

import React,{createContext,useContext,useState} from 'react';
const Context=createContext();
export function LanguageProvider({children}){
 const [lang,setLang]=useState(()=>{const query=new URLSearchParams(location.search).get('lang');return query==='zh'||query==='en'?query:localStorage.getItem('nervus-language')||'en'});
 const change=()=>{const next=lang==='en'?'zh':'en';setLang(next);localStorage.setItem('nervus-language',next);document.documentElement.lang=next==='zh'?'zh-CN':'en';const url=new URL(location.href);url.searchParams.set('lang',next);history.replaceState(null,'',url)};
 React.useEffect(()=>{document.documentElement.lang=lang==='zh'?'zh-CN':'en'},[lang]);
 return <Context.Provider value={{lang,t:(en,zh)=>lang==='zh'?zh:en,local:v=>typeof v==='object'?v[lang]:v,toggle:change}}>{children}</Context.Provider>
}
export const useLanguage=()=>useContext(Context);

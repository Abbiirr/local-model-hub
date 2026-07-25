/// <reference types="vite/client" />

// CSS modules: typed as a string→string map.
declare module '*.module.css' {
  const classes: { readonly [key: string]: string };
  export default classes;
}

// Plain CSS side-effect imports (e.g. theme.css).
declare module '*.css';

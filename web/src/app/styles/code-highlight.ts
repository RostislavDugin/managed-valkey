import hljs from 'highlight.js/lib/core';
import go from 'highlight.js/lib/languages/go';
import javascript from 'highlight.js/lib/languages/javascript';
import python from 'highlight.js/lib/languages/python';
import typescript from 'highlight.js/lib/languages/typescript';
import { createHighlightJsAdapter } from '@mantine/code-highlight';

hljs.registerLanguage('go', go);
hljs.registerLanguage('javascript', javascript);
hljs.registerLanguage('python', python);
hljs.registerLanguage('typescript', typescript);

export const codeHighlightAdapter = createHighlightJsAdapter(hljs);

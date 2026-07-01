import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  integrations: [
    starlight({
      title: 'image-patch-operator',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/pinclr/image-patcher' },
      ],
      defaultLocale: 'root',
      locales: {
        root: {
          label: 'English',
          lang: 'en',
        },
        'zh-cn': {
          label: '中文',
          lang: 'zh-CN',
        },
      },
      sidebar: [
        {
          label: 'Guides',
          translations: { 'zh-cn': '指南' },
          items: [
            {
              label: 'Overview',
              link: '/',
              translations: { 'zh-cn': '概览' },
            },
            {
              label: 'Getting Started',
              link: '/getting-started/',
              translations: { 'zh-cn': '快速开始' },
            },
          ],
        },
        {
          label: 'Reference',
          translations: { 'zh-cn': '参考' },
          items: [
            {
              label: 'CRD Reference',
              link: '/crd-reference/',
              translations: { 'zh-cn': 'CRD 参考' },
            },
            {
              label: 'Advanced Features',
              link: '/advanced/',
              translations: { 'zh-cn': '高级功能' },
            },
          ],
        },
        {
          label: 'Contributing',
          translations: { 'zh-cn': '贡献' },
          items: [
            {
              label: 'Development Guide',
              link: '/development/',
              translations: { 'zh-cn': '开发指南' },
            },
          ],
        },
      ],
    }),
  ],
});

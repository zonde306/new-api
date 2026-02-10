/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import React, { useRef, useEffect, useCallback } from 'react';
import { Toast, Button, Typography } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { Image, UploadCloud, X } from 'lucide-react';
import { usePlayground } from '../../contexts/PlaygroundContext';

const CustomInputRender = (props) => {
  const { t } = useTranslation();
  const {
    onPasteImage,
    onRemoveImage,
    onClearImages,
    imageEnabled,
    imageAttachments,
    customRequestMode,
  } = usePlayground();
  const { detailProps } = props;
  const { clearContextNode, uploadNode, inputNode, sendNode, onClick } =
    detailProps;
  const containerRef = useRef(null);
  const fileInputRef = useRef(null);

  const canAttachImages = imageEnabled && !customRequestMode;

  const readImageFile = useCallback(
    (file) =>
      new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = (event) => resolve(event.target?.result || '');
        reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(file);
      }),
    [],
  );

  const handleFileSelection = useCallback(
    async (event) => {
      const files = Array.from(event.target?.files || []);
      if (!files.length) {
        return;
      }
      if (!canAttachImages) {
        Toast.warning({
          content: customRequestMode
            ? t('自定义模式下无法附加图片')
            : t('请先在设置中启用图片功能'),
          duration: 3,
        });
        event.target.value = '';
        return;
      }

      try {
        const results = await Promise.all(
          files.map((file) => readImageFile(file)),
        );
        results
          .filter(Boolean)
          .forEach((base64) => {
            onPasteImage?.(base64);
          });
        if (results.length > 0) {
          Toast.success({
            content: t('图片已添加'),
            duration: 2,
          });
        }
      } catch (error) {
        console.error('Failed to upload images:', error);
        Toast.error({
          content: t('添加图片失败'),
          duration: 2,
        });
      } finally {
        event.target.value = '';
      }
    },
    [canAttachImages, customRequestMode, onPasteImage, readImageFile, t],
  );

  const handleUploadClick = useCallback(
    (event) => {
      event?.stopPropagation();
      if (!canAttachImages) {
        Toast.warning({
          content: customRequestMode
            ? t('自定义模式下无法附加图片')
            : t('请先在设置中启用图片功能'),
          duration: 3,
        });
        return;
      }
      fileInputRef.current?.click();
    },
    [canAttachImages, customRequestMode, t],
  );

  const handlePaste = useCallback(
    async (e) => {
      const items = e.clipboardData?.items;
      if (!items) return;

      for (let i = 0; i < items.length; i++) {
        const item = items[i];

        if (item.type.indexOf('image') !== -1) {
          e.preventDefault();
          const file = item.getAsFile();

          if (file) {
            try {
              if (!canAttachImages) {
                Toast.warning({
                  content: customRequestMode
                    ? t('自定义模式下无法附加图片')
                    : t('请先在设置中启用图片功能'),
                  duration: 3,
                });
                return;
              }

              const base64 = await readImageFile(file);

              if (onPasteImage) {
                onPasteImage(base64);
                Toast.success({
                  content: t('图片已添加'),
                  duration: 2,
                });
              } else {
                Toast.error({
                  content: t('无法添加图片'),
                  duration: 2,
                });
              }
            } catch (error) {
              console.error('Failed to paste image:', error);
              Toast.error({
                content: t('粘贴图片失败'),
                duration: 2,
              });
            }
          }
          break;
        }
      }
    },
    [canAttachImages, customRequestMode, onPasteImage, readImageFile, t],
  );

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    container.addEventListener('paste', handlePaste);
    return () => {
      container.removeEventListener('paste', handlePaste);
    };
  }, [handlePaste]);

  // 清空按钮
  const styledClearNode = clearContextNode
    ? React.cloneElement(clearContextNode, {
        className: `!rounded-full !bg-gray-100 hover:!bg-red-500 hover:!text-white flex-shrink-0 transition-all ${clearContextNode.props.className || ''}`,
        style: {
          ...clearContextNode.props.style,
          width: '32px',
          height: '32px',
          minWidth: '32px',
          padding: 0,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
        },
      })
    : null;

  // 发送按钮
  const styledSendNode = React.cloneElement(sendNode, {
    className: `!rounded-full !bg-purple-500 hover:!bg-purple-600 flex-shrink-0 transition-all ${sendNode.props.className || ''}`,
    style: {
      ...sendNode.props.style,
      width: '32px',
      height: '32px',
      minWidth: '32px',
      padding: 0,
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
    },
  });

  return (
    <div className='p-2 sm:p-4' ref={containerRef}>
      {imageAttachments.length > 0 && (
        <div
          className='mb-2 rounded-xl border border-dashed border-blue-200 bg-blue-50/60 p-2 space-y-2'
          onClick={onClick}
        >
          <div className='flex items-center justify-between'>
            <div className='flex items-center gap-2'>
              <Image size={14} className='text-blue-500' />
              <Typography.Text className='text-xs text-blue-700'>
                {t('已附加图片')} {imageAttachments.length} {t('张')}
              </Typography.Text>
            </div>
            <Button
              size='small'
              theme='borderless'
              type='tertiary'
              icon={<X size={12} />}
              onClick={(event) => {
                event.stopPropagation();
                onClearImages?.();
              }}
            >
              {t('清空')}
            </Button>
          </div>
          <div className='flex flex-wrap gap-2'>
            {imageAttachments.map((url, index) => (
              <div
                key={`${url}-${index}`}
                className='relative w-14 h-14 rounded-lg overflow-hidden border border-blue-100 bg-white shadow-sm'
              >
                <img
                  src={url}
                  alt={t('已附加图片')}
                  className='w-full h-full object-cover'
                />
                <button
                  type='button'
                  className='absolute -top-2 -right-2 w-5 h-5 rounded-full bg-red-500 text-white flex items-center justify-center shadow'
                  onClick={(event) => {
                    event.stopPropagation();
                    onRemoveImage?.(index);
                  }}
                >
                  <X size={10} />
                </button>
              </div>
            ))}
          </div>
        </div>
      )}
      <div
        className='flex items-center gap-2 sm:gap-3 p-2 bg-gray-50 rounded-xl sm:rounded-2xl shadow-sm hover:shadow-md transition-shadow'
        style={{ border: '1px solid var(--semi-color-border)' }}
        onClick={onClick}
        title={t('支持 Ctrl+V 粘贴图片')}
      >
        {/* 清空对话按钮 - 左边 */}
        {styledClearNode}
        <div className='flex-1'>{inputNode}</div>
        <input
          ref={fileInputRef}
          type='file'
          accept='image/*'
          multiple
          hidden
          onChange={handleFileSelection}
        />
        <Button
          icon={<UploadCloud size={14} />}
          size='small'
          theme='borderless'
          type='tertiary'
          onClick={handleUploadClick}
          className={`!rounded-full ${canAttachImages ? '!text-blue-500 hover:!bg-blue-50' : '!text-gray-300 !cursor-not-allowed'} !w-7 !h-7 !p-0 transition-all`}
          aria-label={t('上传图片')}
        />
        {/* 发送按钮 - 右边 */}
        {styledSendNode}
      </div>
    </div>
  );
};

export default CustomInputRender;
